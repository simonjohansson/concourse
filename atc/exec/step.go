package exec

import (
	"context"
	"io"

	"github.com/concourse/concourse/atc"
	"github.com/concourse/concourse/atc/exec/artifact"
)

//go:generate counterfeiter . Step

// A Step is an object that can be executed, whose result (e.g. Success) can be
// collected, and whose dependent resources (e.g. Containers, Volumes) can be
// released, allowing them to expire.
type Step interface {
	// Run is called when it's time to execute the step. It should watch for the
	// given context to be canceled in the event that the build is aborted or the
	// step times out, and be sure to propagate the (context.Context).Err().
	//
	// Steps wrapping other steps should be careful to propagate the context.
	//
	// Steps must be idempotent. Each step is responsible for handling its own
	// idempotency.
	Run(context.Context, RunState) error

	// Succeeded is true when the Step succeeded, and false otherwise.
	// Succeeded is not guaranteed to be truthful until after you run Run()
	Succeeded() bool
}

//go:generate counterfeiter . RunState

type InputHandler func(io.ReadCloser) error
type OutputHandler func(io.Writer) error

type RunState interface {
	Artifacts() *artifact.Repository

	Result(atc.PlanID, interface{}) bool
	StoreResult(atc.PlanID, interface{})
}

// ExitStatus is the resulting exit code from the process that the step ran.
// Typically if the ExitStatus result is 0, the Success result is true.
type ExitStatus int

// VersionInfo is the version and metadata of a resource that was fetched or
// produced. It is used by Put and Get.
type VersionInfo struct {
	Version  atc.Version
	Metadata []atc.MetadataField
}
