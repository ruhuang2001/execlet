package dockerbackend

import (
	"context"
	_ "embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/daemon"
)

//go:embed supervisor.py
var supervisor string

// BackgroundCommands declares support for the managed-execution protocol.
// Its images must include Linux Python 3; a missing helper fails before an
// accepted command, never falls back to numeric PID signaling.
func (*Backend) BackgroundCommands() bool { return true }

func (b *Backend) managedCommand(ctx context.Context, containerID, path, token string, spec daemon.ExecSpec) ([]string, string, error) {
	env := make(map[string]string, len(spec.Env))
	for _, item := range spec.Env {
		key, value, _ := strings.Cut(item, "=")
		env[key] = value
	}
	payload, err := json.Marshal(struct {
		Command   []string          `json:"command"`
		Cwd       string            `json:"cwd"`
		Env       map[string]string `json:"env"`
		TimeoutMS int64             `json:"timeout_ms"`
	}{spec.Command, spec.Dir, env, *spec.TimeoutMS})
	if err != nil {
		return nil, "", err
	}
	// The archive staging path keeps large command/environment payloads out of
	// Linux's per-argument limit. The supervisor unlinks it before spawning.
	dir, err := b.stageStdin(ctx, containerID, string(payload))
	if err != nil {
		return nil, "", err
	}
	return []string{"python3", "-I", "-c", supervisor, "start", path, token, dir}, dir, nil
}

func (p *process) PID() int { return p.guestPID }

func (p *process) TimedOut() bool { return p.managedTimeout }

func (p *process) Killed() bool { return p.managedKilled }

func (p *process) readManagedOutcome(ctx context.Context) error {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	data, err := p.b.capture(ctx, p.containerID, []string{"cat", p.pidPath + ".result"})
	if err != nil {
		return err
	}
	var result struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	p.managedTimeout = result.Reason == "timeout"
	p.managedKilled = result.Reason == "canceled"
	p.exited = true
	_, _ = p.b.capture(ctx, p.containerID, []string{"rm", "-f", p.pidPath + ".result"})
	return nil
}

func (p *process) readManagedPID(ctx context.Context) error {
	ended := false
	for {
		data, err := p.b.capture(ctx, p.containerID, []string{"cat", p.pidPath})
		if err == nil {
			var identity struct {
				PID int `json:"pid"`
			}
			if err := json.Unmarshal(data, &identity); err != nil {
				return err
			}
			if identity.PID <= 0 {
				return errors.New("missing managed PID")
			}
			p.guestPID = identity.PID
			return nil
		}
		if ended {
			return fmt.Errorf("managed command failed before identity: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			// The first cat may precede publication while the process exits
			// before its response arrives. Read again after this barrier.
			ended = true
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// KillManaged never reconstructs a target from a numeric PID. The supervisor
// owns the child until it closes its unique control channel, before reaping.
func (p *process) KillManaged() (bool, error) { return p.signalManaged(daemon.SignalKill) }

func (p *process) signalManaged(sig daemon.Signal) (bool, error) {
	p.signalMu.Lock()
	defer p.signalMu.Unlock()
	if p.controlToken == "" {
		return false, errors.New("execution has no lifetime-bound supervisor")
	}
	if p.exited {
		return false, nil
	}
	action := "kill"
	if sig == daemon.SignalTerm {
		action = "term"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := p.b.capture(ctx, p.containerID, []string{"python3", "-I", "-c", supervisor, "signal", p.pidPath, p.controlToken, action})
	if err != nil {
		return false, fmt.Errorf("managed signal unconfirmed for PID %s: %w", strconv.Itoa(p.guestPID), err)
	}
	var response struct {
		Signaled *bool  `json:"signaled"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false, err
	}
	if response.Error != "" || response.Signaled == nil {
		return false, errors.New("managed signal unconfirmed")
	}
	return *response.Signaled, nil
}
