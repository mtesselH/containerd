package supervisor

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/containerd/chanotify"
	"github.com/docker/containerd/eventloop"
	"github.com/docker/containerd/runtime"
)

const (
	defaultBufferSize = 2048 // size of queue in eventloop
)

// New returns an initialized Process supervisor.
func New(stateDir string, oom bool) (*Supervisor, error) {
	tasks := make(chan *startTask, 10)
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, err
	}
	machine, err := CollectMachineInformation()
	if err != nil {
		return nil, err
	}
	monitor, err := NewMonitor()
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		stateDir:    stateDir,
		containers:  make(map[string]*containerInfo),
		tasks:       tasks,
		machine:     machine,
		subscribers: make(map[chan Event]struct{}),
		el:          eventloop.NewChanLoop(defaultBufferSize),
		monitor:     monitor,
	}
	if err := setupEventLog(s); err != nil {
		return nil, err
	}
	if oom {
		s.notifier = chanotify.New()
		go func() {
			for id := range s.notifier.Chan() {
				e := NewTask(OOMTaskType)
				e.ID = id.(string)
				s.SendTask(e)
			}
		}()
	}
	// register default event handlers
	s.handlers = map[TaskType]Handler{
		ExecExitTaskType:         &ExecExitTask{s},
		ExitTaskType:             &ExitTask{s},
		StartContainerTaskType:   &StartTask{s},
		DeleteTaskType:           &DeleteTask{s},
		GetContainerTaskType:     &GetContainersTask{s},
		SignalTaskType:           &SignalTask{s},
		AddProcessTaskType:       &AddProcessTask{s},
		UpdateContainerTaskType:  &UpdateTask{s},
		CreateCheckpointTaskType: &CreateCheckpointTask{s},
		DeleteCheckpointTaskType: &DeleteCheckpointTask{s},
		StatsTaskType:            &StatsTask{s},
		UpdateProcessTaskType:    &UpdateProcessTask{s},
	}
	go s.exitHandler()
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

type containerInfo struct {
	container runtime.Container
}

func setupEventLog(s *Supervisor) error {
	if err := readEventLog(s); err != nil {
		return err
	}
	logrus.WithField("count", len(s.eventLog)).Debug("containerd: read past events")
	events := s.Events(time.Time{})
	f, err := os.OpenFile(filepath.Join(s.stateDir, "events.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0755)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	go func() {
		for e := range events {
			s.eventLog = append(s.eventLog, e)
			if err := enc.Encode(e); err != nil {
				logrus.WithField("error", err).Error("containerd: write event to journal")
			}
		}
	}()
	return nil
}

func readEventLog(s *Supervisor) error {
	f, err := os.Open(filepath.Join(s.stateDir, "events.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s.eventLog = append(s.eventLog, e)
	}
	return nil
}

type Supervisor struct {
	// stateDir is the directory on the system to store container runtime state information.
	stateDir   string
	containers map[string]*containerInfo
	handlers   map[TaskType]Handler
	events     chan *Task
	tasks      chan *startTask
	// we need a lock around the subscribers map only because additions and deletions from
	// the map are via the API so we cannot really control the concurrency
	subscriberLock sync.RWMutex
	subscribers    map[chan Event]struct{}
	machine        Machine
	notifier       *chanotify.Notifier
	el             eventloop.EventLoop
	monitor        *Monitor
	eventLog       []Event
}

// Stop closes all tasks and sends a SIGTERM to each container's pid1 then waits for they to
// terminate.  After it has handled all the SIGCHILD events it will close the signals chan
// and exit.  Stop is a non-blocking call and will return after the containers have been signaled
func (s *Supervisor) Stop() {
	// Close the tasks channel so that no new containers get started
	close(s.tasks)
}

// Close closes any open files in the supervisor but expects that Stop has been
// callsed so that no more containers are started.
func (s *Supervisor) Close() error {
	return nil
}

type Event struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	Pid       string    `json:"pid,omitempty"`
	Status    int       `json:"status,omitempty"`
}

// Events returns an event channel that external consumers can use to receive updates
// on container events
func (s *Supervisor) Events(from time.Time) chan Event {
	s.subscriberLock.Lock()
	defer s.subscriberLock.Unlock()
	c := make(chan Event, defaultBufferSize)
	EventSubscriberCounter.Inc(1)
	s.subscribers[c] = struct{}{}
	if !from.IsZero() {
		// replay old event
		for _, e := range s.eventLog {
			if e.Timestamp.After(from) {
				c <- e
			}
		}
	}
	return c
}

// Unsubscribe removes the provided channel from receiving any more events
func (s *Supervisor) Unsubscribe(sub chan Event) {
	s.subscriberLock.Lock()
	defer s.subscriberLock.Unlock()
	delete(s.subscribers, sub)
	close(sub)
	EventSubscriberCounter.Dec(1)
}

// notifySubscribers will send the provided event to the external subscribers
// of the events channel
func (s *Supervisor) notifySubscribers(e Event) {
	s.subscriberLock.RLock()
	defer s.subscriberLock.RUnlock()
	for sub := range s.subscribers {
		// do a non-blocking send for the channel
		select {
		case sub <- e:
		default:
			logrus.WithField("event", e.Type).Warn("containerd: event not sent to subscriber")
		}
	}
}

// Start is a non-blocking call that runs the supervisor for monitoring contianer processes and
// executing new containers.
//
// This event loop is the only thing that is allowed to modify state of containers and processes
// therefore it is save to do operations in the handlers that modify state of the system or
// state of the Supervisor
func (s *Supervisor) Start() error {
	logrus.WithField("stateDir", s.stateDir).Debug("containerd: supervisor running")
	return s.el.Start()
}

// Machine returns the machine information for which the
// supervisor is executing on.
func (s *Supervisor) Machine() Machine {
	return s.machine
}

// SendTask sends the provided event the the supervisors main event loop
func (s *Supervisor) SendTask(evt *Task) {
	TasksCounter.Inc(1)
	s.el.Send(&commonTask{data: evt, sv: s})
}

func (s *Supervisor) exitHandler() {
	for p := range s.monitor.Exits() {
		e := NewTask(ExitTaskType)
		e.Process = p
		s.SendTask(e)
	}
}

func (s *Supervisor) monitorProcess(p runtime.Process) error {
	return s.monitor.Monitor(p)
}

func (s *Supervisor) restore() error {
	dirs, err := ioutil.ReadDir(s.stateDir)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		id := d.Name()
		container, err := runtime.Load(s.stateDir, id)
		if err != nil {
			return err
		}
		processes, err := container.Processes()
		if err != nil {
			return err
		}
		ContainersCounter.Inc(1)
		s.containers[id] = &containerInfo{
			container: container,
		}
		logrus.WithField("id", id).Debug("containerd: container restored")
		var exitedProcesses []runtime.Process
		for _, p := range processes {
			if _, err := p.ExitStatus(); err == nil {
				exitedProcesses = append(exitedProcesses, p)
			} else {
				if err := s.monitorProcess(p); err != nil {
					return err
				}
			}
		}
		if len(exitedProcesses) > 0 {
			// sort processes so that init is fired last because that is how the kernel sends the
			// exit events
			sort.Sort(&processSorter{exitedProcesses})
			for _, p := range exitedProcesses {
				e := NewTask(ExitTaskType)
				e.Process = p
				s.SendTask(e)
			}
		}
	}
	return nil
}

type processSorter struct {
	processes []runtime.Process
}

func (s *processSorter) Len() int {
	return len(s.processes)
}

func (s *processSorter) Swap(i, j int) {
	s.processes[i], s.processes[j] = s.processes[j], s.processes[i]
}

func (s *processSorter) Less(i, j int) bool {
	return s.processes[j].ID() == "init"
}
