package daemon

import (
	"context"
	"time"
)

type sampler interface {
	Liveness(ctx context.Context, containerID string) (Liveness, error)
	OOMKillCount(ctx context.Context, containerID string) (int64, error)
}

type ExecOutcome struct {
	Reason      Reason
	ExitCode    int
	SandboxLost bool

	sampled []Liveness
	elapsed time.Duration
}

func classify(ctx context.Context, s sampler, containerID string, oomBaseline int64, exitCode int, intent *intentRecord, sleep SleepFunc) ExecOutcome {
	return classifyWithAttribution(ctx, s, containerID, oomBaseline, exitCode, intent, sleep, true)
}

func classifyWithAttribution(ctx context.Context, s sampler, containerID string, oomBaseline int64, exitCode int, intent *intentRecord, sleep SleepFunc, attributeOOM bool) ExecOutcome {
	if intent != nil {
		switch intent.reason {
		case ReasonCanceled:
			return ExecOutcome{Reason: ReasonCanceled, ExitCode: exitCode}
		case ReasonTimeout:
			return ExecOutcome{Reason: ReasonTimeout, ExitCode: exitCode}
		}
	}

	if attributeOOM && oomBaseline >= 0 {
		if current, err := s.OOMKillCount(ctx, containerID); err == nil && current > oomBaseline {
			return ExecOutcome{Reason: ReasonOOM, ExitCode: exitCode}
		}
	}

	ambiguous := exitCode == 137
	samples := 1
	if ambiguous {
		samples = ambiguousSamples
	}

	liveness := LivenessRunning
	var lastErr error
	observed := false
	var sampled []Liveness
	started := time.Now()
	for i := 0; i < samples; i++ {
		if i > 0 && !sleep(ctx, ambiguousInterval) {
			break
		}
		answer, err := s.Liveness(ctx, containerID)
		if err != nil {
			lastErr = err
			continue
		}
		lastErr = nil
		previous := liveness
		liveness = answer
		observed = true
		if ambiguous {
			sampled = append(sampled, answer)
		}
		if i+1 >= ambiguousMinSamples && answer == LivenessStopped && previous == LivenessStopped {
			break
		}
	}
	elapsed := time.Since(started)
	withTrace := func(out ExecOutcome) ExecOutcome {
		out.sampled = sampled
		out.elapsed = elapsed
		return out
	}

	if !observed && lastErr != nil {
		return withTrace(ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true})
	}
	if !liveness.Alive() {
		return withTrace(ExecOutcome{Reason: ReasonEvicted, ExitCode: exitCode, SandboxLost: true})
	}

	if !attributeOOM && exitCode == 137 {
		return withTrace(ExecOutcome{Reason: ReasonUnknown, ExitCode: exitCode})
	}
	return withTrace(ExecOutcome{Reason: ReasonExited, ExitCode: exitCode})
}
