package pipelines

import (
	"time"

	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/engine"
	"github.com/concourse/atc/radar"
	"github.com/concourse/atc/resource"
	"github.com/concourse/atc/scheduler"
	"github.com/concourse/atc/scheduler/factory"
	"github.com/pivotal-golang/clock"
)

//go:generate counterfeiter . RadarSchedulerFactory

type RadarSchedulerFactory interface {
	BuildScannerFactory(pipelineDB db.PipelineDB, externalURL string) radar.ScannerFactory
	BuildScheduler(pipelineDB db.PipelineDB, externalURL string) scheduler.BuildScheduler
}

type radarSchedulerFactory struct {
	tracker  resource.Tracker
	interval time.Duration
	engine   engine.Engine
	db       db.DB
}

func NewRadarSchedulerFactory(
	tracker resource.Tracker,
	interval time.Duration,
	engine engine.Engine,
	db db.DB,
) RadarSchedulerFactory {
	return &radarSchedulerFactory{
		tracker:  tracker,
		interval: interval,
		engine:   engine,
		db:       db,
	}
}

func (rsf *radarSchedulerFactory) BuildScannerFactory(pipelineDB db.PipelineDB, externalURL string) radar.ScannerFactory {
	return radar.NewScannerFactory(rsf.tracker, rsf.interval, pipelineDB, clock.NewClock(), externalURL)
}

func (rsf *radarSchedulerFactory) BuildScheduler(pipelineDB db.PipelineDB, externalURL string) scheduler.BuildScheduler {
	scanner := radar.NewResourceScanner(
		clock.NewClock(),
		rsf.tracker,
		rsf.interval,
		pipelineDB,
		externalURL,
	)
	return &scheduler.Scheduler{
		PipelineDB: pipelineDB,
		BuildsDB:   rsf.db,
		Factory: factory.NewBuildFactory(
			pipelineDB.GetPipelineName(),
			atc.NewPlanFactory(time.Now().Unix()),
		),
		Engine:  rsf.engine,
		Scanner: scanner,
	}
}
