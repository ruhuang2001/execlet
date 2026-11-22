package dockerapi

type ContainerInspect struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	ID         string         `json:"Id"`
	Name       string         `json:"Name"`
	State      ContainerState `json:"State"`
	HostConfig HostConfig     `json:"HostConfig"`
}

type ContainerState struct {
	Status    string `json:"Status"`
	Running   bool   `json:"Running"`
	Paused    bool   `json:"Paused"`
	OOMKilled bool   `json:"OOMKilled"`
	ExitCode  int    `json:"ExitCode"`
	Pid       int    `json:"Pid"`
}
