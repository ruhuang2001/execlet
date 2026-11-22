package dockerapi

type ContainerInspect struct {
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
