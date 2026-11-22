package dockerapi

// ContainerConfig is the body of POST /containers/create. Only the fields the
// sandbox backend sets are modelled; the Engine API ignores the rest.
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig carries the resource limits and the security floor every sandbox
// container is created with. MemorySwap is set equal to Memory so a sandbox
// cannot use swap to exceed its declared memory limit.
//
// PidsLimit is a pointer because Docker distinguishes "unset" from zero: zero
// means unlimited, and an unset field would let the daemon's own default
// apply. The sandbox always sets it.
type HostConfig struct {
	Memory      int64             `json:"Memory,omitempty"`
	MemorySwap  int64             `json:"MemorySwap,omitempty"`
	NanoCpus    int64             `json:"NanoCpus,omitempty"`
	PidsLimit   *int64            `json:"PidsLimit,omitempty"`
	CapDrop     []string          `json:"CapDrop,omitempty"`
	SecurityOpt []string          `json:"SecurityOpt,omitempty"`
	NetworkMode string            `json:"NetworkMode,omitempty"`
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`
	// Init runs Docker's init process as PID 1 instead of the container's
	// command. The sandbox needs it because PID 1 is what reaps orphaned
	// grandchildren, and a pids limit makes unreaped zombies permanent lost
	// task slots.
	Init *bool `json:"Init,omitempty"`
}

// Container is one entry of GET /containers/json.
type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// ExecInspect is the body of GET /exec/{id}/json. ExitCode is null until the
// process exits.
type ExecInspect struct {
	ID          string `json:"ID"`
	Running     *bool  `json:"Running"` // missing/null is not confirmation of exit
	ExitCode    *int   `json:"ExitCode"`
	Pid         int    `json:"Pid"`
	ContainerID string `json:"ContainerID"`
}

// Stopped distinguishes a confirmed exit from a not-yet-started exec with a null exit code.
func (e ExecInspect) Stopped(execID string) bool {
	return e.ID == execID && e.Running != nil && !*e.Running && e.ExitCode != nil
}

// ExecConfig is the body of an exec create. Env is additive to the container's
// own environment: a name here overrides the container's value and every other
// variable stays visible.
//
// Env also reaches the target a second way: the backend repeats it in the
// wrapper argv, which re-applies it with env(1) after the shell has started.
// That is not redundancy. A shell imports only the names it accepts, so dash
// drops MY-VAR and CONFIG.KEY and both shells rewrite IFS, PWD and SHLVL; Env
// alone therefore does not satisfy the contract for every accepted name.
//
// There is deliberately no WorkingDir. The runtime applies it before the
// command starts, so an unusable directory fails the exec as a startup error:
// exit 127 with the message on stdout, which is indistinguishable from a
// missing command and reads like program output. The backend puts the
// directory in the command wrapper instead, and reports a bad directory as
// exit 126 with a marker on stderr.
type ExecConfig struct {
	User         string   `json:"User,omitempty"`
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
}

type execStart struct {
	Detach bool `json:"Detach"`
	Tty    bool `json:"Tty"`
}
