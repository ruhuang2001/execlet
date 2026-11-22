package dockerapi

import (
	"encoding/json"
	"testing"
)

func TestExecInspectConfirmsExactStoppedExec(t *testing.T) {
	for payload, want := range map[string]bool{
		`{}`:                        false,
		`{"ID":"a","Running":null}`: false,
		`{"ID":"a","Running":true}`: false,
		`{"ID":"other","Running":false,"ExitCode":0}`: false,
		`{"ID":"a","Running":false}`:                  false,
		`{"ID":"a","Running":false,"ExitCode":null}`:  false,
		`{"ID":"a","Running":false,"ExitCode":0}`:     true,
	} {
		var state ExecInspect
		if err := json.Unmarshal([]byte(payload), &state); err != nil {
			t.Fatal(err)
		}
		if got := state.Stopped("a"); got != want {
			t.Errorf("Stopped(%s) = %t, want %t", payload, got, want)
		}
	}
}

func TestHostConfigJSONFieldNames(t *testing.T) {
	pids := int64(1024)
	initProcess := true
	data, err := json.Marshal(HostConfig{
		Memory:      512 << 20,
		MemorySwap:  512 << 20,
		PidsLimit:   &pids,
		CapDrop:     []string{"NET_RAW"},
		SecurityOpt: []string{"no-new-privileges"},
		Init:        &initProcess,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"Memory", "MemorySwap", "PidsLimit", "CapDrop", "SecurityOpt", "Init"} {
		if _, ok := got[key]; !ok {
			t.Errorf("%s is missing from %s", key, data)
		}
	}
	if _, ok := got["PidsLimit"].(float64); !ok {
		t.Errorf("PidsLimit = %T, want a JSON number", got["PidsLimit"])
	}
}

func TestHostConfigOmitsUnsetLimits(t *testing.T) {
	data, err := json.Marshal(HostConfig{Memory: 64 << 20})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["PidsLimit"]; ok {
		t.Errorf("unset PidsLimit is serialized: %s", data)
	}
}
