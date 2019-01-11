package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/lager"
	"github.com/concourse/baggageclaim"
	"github.com/concourse/concourse/atc/creds"
	"github.com/concourse/concourse/atc/db"
	"github.com/concourse/concourse/atc/db/lock"
	"github.com/concourse/concourse/atc/metric"
)

const creatingContainerRetryDelay = 1 * time.Second

func NewContainerProvider(
	gardenClient garden.Client,
	volumeClient VolumeClient,
	dbWorker db.Worker,
	imageFactory ImageFactory,
	dbVolumeRepository db.VolumeRepository,
	dbTeamFactory db.TeamFactory,
	lockFactory lock.LockFactory,
) ContainerProvider {

	return &containerProvider{
		gardenClient:       gardenClient,
		volumeClient:       volumeClient,
		imageFactory:       imageFactory,
		dbVolumeRepository: dbVolumeRepository,
		dbTeamFactory:      dbTeamFactory,
		lockFactory:        lockFactory,
		httpProxyURL:       dbWorker.HTTPProxyURL(),
		httpsProxyURL:      dbWorker.HTTPSProxyURL(),
		noProxy:            dbWorker.NoProxy(),
		worker:             dbWorker,
	}
}

//go:generate counterfeiter . ContainerProvider

type ContainerProvider interface {
	FindCreatedContainerByHandle(
		logger lager.Logger,
		handle string,
		teamID int,
	) (Container, bool, error)

	FindOrCreateContainer(
		ctx context.Context,
		logger lager.Logger,
		owner db.ContainerOwner,
		delegate ImageFetchingDelegate,
		metadata db.ContainerMetadata,
		containerSpec ContainerSpec,
		workerSpec WorkerSpec,
		resourceTypes creds.VersionedResourceTypes,
		image Image,
	) (Container, error)
}

// TODO: Remove the ImageFactory from the containerProvider.
// Currently, the imageFactory is only needed to create a garden
// worker in createGardenContainer. Creating a garden worker here
// is cyclical because the garden worker contains a containerProvider.
// There is an ongoing refactor that is attempting to fix this.
type containerProvider struct {
	gardenClient       garden.Client
	volumeClient       VolumeClient
	imageFactory       ImageFactory
	dbVolumeRepository db.VolumeRepository
	dbTeamFactory      db.TeamFactory

	lockFactory lock.LockFactory

	worker        db.Worker
	httpProxyURL  string
	httpsProxyURL string
	noProxy       string
}

// If a created container exists, a garden.Container must also exist
// so this method will find it, create the corresponding worker.Container
// and return it.
// If no created container exists, FindOrCreateContainer will go through
// the container creation flow i.e. find or create a CreatingContainer,
// create the garden.Container and then the CreatedContainer
func (p *containerProvider) FindOrCreateContainer(
	ctx context.Context,
	logger lager.Logger,
	owner db.ContainerOwner,
	delegate ImageFetchingDelegate,
	metadata db.ContainerMetadata,
	containerSpec ContainerSpec,
	workerSpec WorkerSpec,
	resourceTypes creds.VersionedResourceTypes,
	image Image,
) (Container, error) {

	var gardenContainer garden.Container

	creatingContainer, createdContainer, err := p.worker.FindContainerOnWorker(
		owner,
	)
	if err != nil {
		logger.Error("failed-to-find-container-in-db", err)
		return nil, err
	}

	if createdContainer != nil {
		logger = logger.WithData(lager.Data{"container": createdContainer.Handle()})

		logger.Debug("found-created-container-in-db")

		gardenContainer, err = p.gardenClient.Lookup(createdContainer.Handle())
		if err != nil {
			logger.Error("failed-to-lookup-created-container-in-garden", err)
			return nil, err
		}

		return p.constructGardenWorkerContainer(
			logger,
			createdContainer,
			gardenContainer,
		)
	}

	if creatingContainer == nil {
		logger.Debug("creating-container-in-db")

		creatingContainer, err = p.worker.CreateContainer(
			owner,
			metadata,
		)
		if err != nil {
			logger.Error("failed-to-create-container-in-db", err)
			return nil, err
		}

		logger = logger.WithData(lager.Data{"container": creatingContainer.Handle()})
		logger.Debug("created-creating-container-in-db")
	} else {
		logger = logger.WithData(lager.Data{"container": creatingContainer.Handle()})
		logger.Debug("found-creating-container-in-db")
	}

	gardenContainer, err = p.gardenClient.Lookup(creatingContainer.Handle())
	if err != nil {
		if _, ok := err.(garden.ContainerNotFoundError); !ok {
			logger.Error("failed-to-lookup-creating-container-in-garden", err)
			return nil, err
		}
	}

	if gardenContainer == nil {
		lock, acquired, err := p.lockFactory.Acquire(logger, lock.NewContainerCreatingLockID(creatingContainer.ID()))
		if err != nil {
			logger.Error("failed-to-acquire-container-creating-lock", err)
			return nil, err
		}

		if !acquired {
			time.Sleep(creatingContainerRetryDelay)
			return nil, nil
		}

		defer lock.Release()

		logger.Debug("fetching-image")

		fetchedImage, err := image.FetchForContainer(ctx, logger, creatingContainer)
		if err != nil {
			creatingContainer.Failed()
			logger.Error("failed-to-fetch-image-for-container", err)
			return nil, err
		}

		logger.Debug("creating-container-in-garden")

		gardenContainer, err = p.createGardenContainer(
			logger,
			creatingContainer,
			containerSpec,
			fetchedImage,
		)
		if err != nil {
			_, failedErr := creatingContainer.Failed()
			if failedErr != nil {
				logger.Error("failed-to-mark-container-as-failed", err)
			}
			metric.FailedContainers.Inc()

			logger.Error("failed-to-create-container-in-garden", err)
			return nil, err
		}

		metric.ContainersCreated.Inc()

		logger.Debug("created-container-in-garden")
	} else {
		logger.Debug("found-created-container-in-garden")
	}

	createdContainer, err = creatingContainer.Created()
	if err != nil {
		logger.Error("failed-to-mark-container-as-created", err)

		_ = p.gardenClient.Destroy(creatingContainer.Handle())

		return nil, err
	}

	logger.Debug("created-container-in-db")

	return p.constructGardenWorkerContainer(
		logger,
		createdContainer,
		gardenContainer,
	)
}

func (p *containerProvider) FindCreatedContainerByHandle(
	logger lager.Logger,
	handle string,
	teamID int,
) (Container, bool, error) {
	gardenContainer, err := p.gardenClient.Lookup(handle)
	if err != nil {
		if _, ok := err.(garden.ContainerNotFoundError); ok {
			logger.Info("container-not-found")
			return nil, false, nil
		}

		logger.Error("failed-to-lookup-on-garden", err)
		return nil, false, err
	}

	createdContainer, found, err := p.dbTeamFactory.GetByID(teamID).FindCreatedContainerByHandle(handle)
	if err != nil {
		logger.Error("failed-to-lookup-in-db", err)
		return nil, false, err
	}

	if !found {
		return nil, false, nil
	}

	createdVolumes, err := p.dbVolumeRepository.FindVolumesForContainer(createdContainer)
	if err != nil {
		return nil, false, err
	}

	container, err := newGardenWorkerContainer(
		logger,
		gardenContainer,
		createdContainer,
		createdVolumes,
		p.gardenClient,
		p.volumeClient,
		p.worker.Name(),
	)

	if err != nil {
		logger.Error("failed-to-construct-container", err)
		return nil, false, err
	}

	return container, true, nil
}

func (p *containerProvider) constructGardenWorkerContainer(
	logger lager.Logger,
	createdContainer db.CreatedContainer,
	gardenContainer garden.Container,
) (Container, error) {
	createdVolumes, err := p.dbVolumeRepository.FindVolumesForContainer(createdContainer)
	if err != nil {
		logger.Error("failed-to-find-container-volumes", err)
		return nil, err
	}

	return newGardenWorkerContainer(
		logger,
		gardenContainer,
		createdContainer,
		createdVolumes,
		p.gardenClient,
		p.volumeClient,
		p.worker.Name(),
	)
}

func (p *containerProvider) createGardenContainer(
	logger lager.Logger,
	creatingContainer db.CreatingContainer,
	spec ContainerSpec,
	fetchedImage FetchedImage,
) (garden.Container, error) {
	var volumeMounts []VolumeMount
	var ioVolumeMounts []VolumeMount

	scratchVolume, err := p.volumeClient.FindOrCreateVolumeForContainer(
		logger,
		VolumeSpec{
			Strategy:   baggageclaim.EmptyStrategy{},
			Privileged: fetchedImage.Privileged,
		},
		creatingContainer,
		spec.TeamID,
		"/scratch",
	)
	if err != nil {
		return nil, err
	}

	volumeMounts = append(volumeMounts, VolumeMount{
		Volume:    scratchVolume,
		MountPath: "/scratch",
	})

	hasSpecDirInInputs := anyMountTo(spec.Dir, getDestinationPathsFromInputs(spec.Inputs))
	hasSpecDirInOutputs := anyMountTo(spec.Dir, getDestinationPathsFromOutputs(spec.Outputs))

	if spec.Dir != "" && !hasSpecDirInOutputs && !hasSpecDirInInputs {
		workdirVolume, volumeErr := p.volumeClient.FindOrCreateVolumeForContainer(
			logger,
			VolumeSpec{
				Strategy:   baggageclaim.EmptyStrategy{},
				Privileged: fetchedImage.Privileged,
			},
			creatingContainer,
			spec.TeamID,
			spec.Dir,
		)
		if volumeErr != nil {
			return nil, volumeErr
		}

		volumeMounts = append(volumeMounts, VolumeMount{
			Volume:    workdirVolume,
			MountPath: spec.Dir,
		})
	}

	worker := NewGardenWorker(
		p.gardenClient,
		p,
		p.volumeClient,
		p.imageFactory,
		p.worker,
		0,
	)

	inputDestinationPaths := make(map[string]bool)

	for _, inputSource := range spec.Inputs {
		var inputVolume Volume

		localVolume, found, err := inputSource.Source().VolumeOn(logger, worker)
		if err != nil {
			return nil, err
		}

		cleanedInputPath := filepath.Clean(inputSource.DestinationPath())

		if found {
			inputVolume, err = p.volumeClient.FindOrCreateCOWVolumeForContainer(
				logger,
				VolumeSpec{
					Strategy:   localVolume.COWStrategy(),
					Privileged: fetchedImage.Privileged,
				},
				creatingContainer,
				localVolume,
				spec.TeamID,
				cleanedInputPath,
			)
			if err != nil {
				return nil, err
			}
		} else {
			inputVolume, err = p.volumeClient.FindOrCreateVolumeForContainer(
				logger,
				VolumeSpec{
					Strategy:   baggageclaim.EmptyStrategy{},
					Privileged: fetchedImage.Privileged,
				},
				creatingContainer,
				spec.TeamID,
				cleanedInputPath,
			)
			if err != nil {
				return nil, err
			}

			destData := lager.Data{
				"dest-volume": inputVolume.Handle(),
				"dest-worker": inputVolume.WorkerName(),
			}
			err = inputSource.Source().StreamTo(logger.Session("stream-to", destData), inputVolume)
			if err != nil {
				return nil, err
			}
		}

		ioVolumeMounts = append(ioVolumeMounts, VolumeMount{
			Volume:    inputVolume,
			MountPath: cleanedInputPath,
		})

		inputDestinationPaths[cleanedInputPath] = true
	}

	for _, outputPath := range spec.Outputs {
		cleanedOutputPath := filepath.Clean(outputPath)

		// reuse volume if output path is the same as input
		if inputDestinationPaths[cleanedOutputPath] {
			continue
		}

		outVolume, volumeErr := p.volumeClient.FindOrCreateVolumeForContainer(
			logger,
			VolumeSpec{
				Strategy:   baggageclaim.EmptyStrategy{},
				Privileged: fetchedImage.Privileged,
			},
			creatingContainer,
			spec.TeamID,
			cleanedOutputPath,
		)
		if volumeErr != nil {
			return nil, volumeErr
		}

		ioVolumeMounts = append(ioVolumeMounts, VolumeMount{
			Volume:    outVolume,
			MountPath: cleanedOutputPath,
		})
	}
	bindMounts := []garden.BindMount{}

	for _, mount := range spec.BindMounts {
		bindMount, found, mountErr := mount.VolumeOn(worker)
		if mountErr != nil {
			return nil, mountErr
		}
		if found {
			bindMounts = append(bindMounts, bindMount)
		}
	}

	sort.Sort(byMountPath(ioVolumeMounts))
	volumeMounts = append(volumeMounts, ioVolumeMounts...)

	for _, mount := range volumeMounts {
		bindMounts = append(bindMounts, garden.BindMount{
			SrcPath: mount.Volume.Path(),
			DstPath: mount.MountPath,
			Mode:    garden.BindMountModeRW,
		})
	}

	gardenProperties := garden.Properties{}

	if spec.User != "" {
		gardenProperties[userPropertyName] = spec.User
	} else {
		gardenProperties[userPropertyName] = fetchedImage.Metadata.User
	}

	env := append(fetchedImage.Metadata.Env, spec.Env...)

	if p.httpProxyURL != "" {
		env = append(env, fmt.Sprintf("http_proxy=%s", p.httpProxyURL))
	}

	if p.httpsProxyURL != "" {
		env = append(env, fmt.Sprintf("https_proxy=%s", p.httpsProxyURL))
	}

	if p.noProxy != "" {
		env = append(env, fmt.Sprintf("no_proxy=%s", p.noProxy))
	}

	return p.gardenClient.Create(garden.ContainerSpec{
		Handle:     creatingContainer.Handle(),
		RootFSPath: fetchedImage.URL,
		Privileged: fetchedImage.Privileged,
		BindMounts: bindMounts,
		Limits:     spec.Limits.ToGardenLimits(),
		Env:        env,
		Properties: gardenProperties,
	})
}

func getDestinationPathsFromInputs(inputs []InputSource) []string {
	destinationPaths := make([]string, len(inputs))

	for idx, input := range inputs {
		destinationPaths[idx] = input.DestinationPath()
	}

	return destinationPaths
}

func getDestinationPathsFromOutputs(outputs OutputPaths) []string {
	var (
		idx              = 0
		destinationPaths = make([]string, len(outputs))
	)

	for _, destinationPath := range outputs {
		destinationPaths[idx] = destinationPath
		idx++
	}

	return destinationPaths
}

func anyMountTo(path string, destinationPaths []string) bool {
	for _, destinationPath := range destinationPaths {
		if filepath.Clean(destinationPath) == filepath.Clean(path) {
			return true
		}
	}

	return false
}
