package dockerapi

type ContainerInspect struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Image      string         `json:"Image"`
	ID         string         `json:"Id"`
	Name       string         `json:"Name"`
	State      ContainerState `json:"State"`
	HostConfig HostConfig     `json:"HostConfig"`
	Mounts     []MountPoint   `json:"Mounts"`
}

type MountPoint struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          *bool  `json:"RW"`
}

type ContainerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	Paused    bool   `json:"Paused"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  int    `json:"ExitCode"`
	Pid       int    `json:"Pid"`
}
