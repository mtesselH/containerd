package runtime

import (
	"errors"
	"time"

	"github.com/opencontainers/specs"
)

var (
	ErrNotChildProcess       = errors.New("containerd: not a child process for container")
	ErrInvalidContainerType  = errors.New("containerd: invalid container type for runtime")
	ErrCheckpointNotExists   = errors.New("containerd: checkpoint does not exist for container")
	ErrCheckpointExists      = errors.New("containerd: checkpoint already exists")
	ErrContainerExited       = errors.New("containerd: container has exited")
	ErrTerminalsNotSupported = errors.New("containerd: terminals are not supported for runtime")
	ErrProcessNotExited      = errors.New("containerd: process has not exited")
	ErrProcessExited         = errors.New("containerd: process has exited")

	errNotImplemented = errors.New("containerd: not implemented")
)

const (
	ExitFile       = "exit"
	ExitStatusFile = "exitStatus"
	StateFile      = "state.json"
	ControlFile    = "control"
	InitProcessID  = "init"
)

type State string

const (
	Paused  = State("paused")
	Running = State("running")
)

type state struct {
	Bundle string   `json:"bundle"`
	Labels []string `json:"labels"`
	Stdin  string   `json:"stdin"`
	Stdout string   `json:"stdout"`
	Stderr string   `json:"stderr"`
}

type ProcessState struct {
	specs.Process
	Exec       bool   `json:"exec"`
	Checkpoint string `json:"checkpoint"`
	RootUID    int    `json:"rootUID"`
	RootGID    int    `json:"rootGID"`
	Stdin      string `json:"containerdStdin"`
	Stdout     string `json:"containerdStdout"`
	Stderr     string `json:"containerdStderr"`
}

type Stat struct {
	// Timestamp is the time that the statistics where collected
	Timestamp time.Time
	// Data is the raw stats
	// TODO: it is currently an interface because we don't know what type of exec drivers
	// we will have or what the structure should look like at the moment os the containers
	// can return what they want and we could marshal to json or whatever.
	Data interface{}
}

type Checkpoint struct {
	// Timestamp is the time that checkpoint happened
	Created time.Time `json:"created"`
	// Name is the name of the checkpoint
	Name string `json:"name"`
	// Tcp checkpoints open tcp connections
	Tcp bool `json:"tcp"`
	// UnixSockets persists unix sockets in the checkpoint
	UnixSockets bool `json:"unixSockets"`
	// Shell persists tty sessions in the checkpoint
	Shell bool `json:"shell"`
	// Exit exits the container after the checkpoint is finished
	Exit bool `json:"exit"`
}
