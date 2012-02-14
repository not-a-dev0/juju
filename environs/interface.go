package environs

import (
	"errors"
	"io"
	"launchpad.net/juju/go/schema"
	"launchpad.net/juju/go/state"
)

// A EnvironProvider represents a computing and storage provider.
type EnvironProvider interface {
	// ConfigChecker is used to check sections of the environments.yaml
	// file that specify this provider. The value passed to the Checker is
	// that returned from the yaml parse, of type schema.MapType.
	ConfigChecker() schema.Checker

	// NewEnviron creates a new Environ with
	// the given attributes returned by the ConfigChecker.
	// The name is that given in environments.yaml.
	Open(name string, attributes interface{}) (Environ, error)
}

// Instance represents the provider-specific notion of a machine.
type Instance interface {
	// Id returns a provider-generated identifier for the Instance.
	Id() string
	DNSName() string
}

var ErrMissingInstance = errors.New("some instance ids not found")

// An Environ represents a juju environment as specified
// in the environments.yaml file.
type Environ interface {
	// Bootstrap initializes the state for the environment,
	// possibly starting one or more instances.
	Bootstrap() (*state.Info, error)

	// StateInfo returns information on the state initialized
	// by Bootstrap.
	StateInfo() (*state.Info, error)

	// StartInstance asks for a new instance to be created,
	// associated with the provided machine identifier.
	// TODO add arguments to specify type of new machine.
	StartInstance(machineId int, state *state.Info) (Instance, error)

	// StopInstances shuts down the given instances.
	StopInstances([]Instance) error

	// Instances returns a slice of instances corresponding to
	// the given instance ids. If some (but not all) of the instances are not
	// found, the returned slice will have nil Inststances in those
	// slots, and ErrMissingInstance will be returned.
	Instances(ids []string) ([]Instance, error)

	// Put reads from r and writes to the given file in the
	// environment's storage. The length must give the total
	// length of the file.
	PutFile(file string, r io.Reader, length int64) error

	// Get opens the given file in the environment's storage
	// and returns a ReadCloser that can be used to read its
	// contents. It is the caller's responsibility to close it
	// after use.
	GetFile(file string) (io.ReadCloser, error)

	// RemoveFile removes the given file from the environment's storage.
	// It is not an error to remove a file that does not exist.
	RemoveFile(file string) error

	// Destroy shuts down all known machines and destroys the
	// rest of the environment. A list of instances known to
	// be part of the environment can be given with insts.
	Destroy(insts []Instance) error
}
