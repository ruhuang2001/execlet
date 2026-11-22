package dockerapi

type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

type HostConfig struct {
	Runtime     string            `json:"Runtime,omitempty"`
	Memory      int64             `json:"Memory,omitempty"`
	MemorySwap  int64             `json:"MemorySwap,omitempty"`
	NanoCpus    int64             `json:"NanoCpus,omitempty"`
	PidsLimit   *int64            `json:"PidsLimit,omitempty"`
	CapDrop     []string          `json:"CapDrop,omitempty"`
	SecurityOpt []string          `json:"SecurityOpt,omitempty"`
	NetworkMode string            `json:"NetworkMode,omitempty"`
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`
	Init        *bool             `json:"Init,omitempty"`
}

type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

type ExecInspect struct {
	ID          string `json:"ID"`
	Running     *bool  `json:"Running"`
	ExitCode    *int   `json:"ExitCode"`
	Pid         int    `json:"Pid"`
	ContainerID string `json:"ContainerID"`
}

func (e ExecInspect) Stopped(execID string) bool {
	return e.ID == execID && e.Running != nil && !*e.Running && e.ExitCode != nil
}

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
