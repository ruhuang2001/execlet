package daemon

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"
)

type EventType string

const (
	EventStdout    EventType = "stdout"
	EventStderr    EventType = "stderr"
	EventTruncated EventType = "truncated"
	EventComplete  EventType = "complete"
)

type Event struct {
	ID         uint64
	Type       EventType
	Data       []byte
	LimitBytes int
	Execution  *Execution
}

type eventLog struct {
	mu      sync.Mutex
	changed chan struct{}

	keepTail                   bool
	events                     []Event
	nextID                     uint64
	stdoutOffset, stderrOffset int64
	retained                   int
	outputEvents               int
	truncated                  bool
	complete                   bool
	stdout                     strings.Builder
	stderr                     strings.Builder
	indexed                    int
	onFirstOutput              func()
}

func newEventLog() *eventLog {
	return &eventLog{changed: make(chan struct{})}
}

func (l *eventLog) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *eventLog) appendOutput(kind EventType, chunk []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.complete || len(chunk) == 0 || (l.truncated && !l.keepTail) {
		return
	}
	for len(chunk) > 0 {
		n := min(len(chunk), MaxEventDataBytes)
		if n < len(chunk) {
			if cut := runeBoundary(chunk, n); cut > 0 {
				n = cut
			}
		}
		if l.keepTail {
			l.makeRoomForTailLocked(n)
			l.appendOutputLocked(kind, chunk[:n])
			chunk = chunk[n:]
			continue
		}
		if l.outputEvents >= MaxOutputEvents {
			l.truncateLocked()
			return
		}
		if room := MaxOutputBytes - l.retained; room < n {
			if room <= 0 {
				l.truncateLocked()
				return
			}
			n = room
			if n < len(chunk) {
				if cut := runeBoundary(chunk, n); cut > 0 {
					n = cut
				} else if _, size := utf8.DecodeRune(chunk); size > n {
					n = 0
				}
			}
			if n > 0 {
				l.appendOutputLocked(kind, chunk[:n])
			}
			l.truncateLocked()
			return
		}
		l.appendOutputLocked(kind, chunk[:n])
		chunk = chunk[n:]
	}
}

func (l *eventLog) makeRoomForTailLocked(size int) {
	if l.retained+size <= MaxOutputBytes && l.outputEvents < MaxOutputEvents {
		return
	}
	first := 0
	for l.retained+size > MaxOutputBytes || l.outputEvents >= MaxOutputEvents {
		l.dropOutputLocked(first)
		first++
	}
	l.markPrefixLocked(first)
}

func (l *eventLog) dropOutputLocked(i int) {
	ev := &l.events[i]
	switch ev.Type {
	case EventStdout:
		l.stdoutOffset += int64(len(ev.Data))
	case EventStderr:
		l.stderrOffset += int64(len(ev.Data))
	default:
		return
	}
	l.retained -= len(ev.Data)
	ev.Data = nil
	l.outputEvents--
}

func (l *eventLog) markPrefixLocked(first int) {
	marker := l.events[first-1].ID
	l.events = l.events[first-1:]
	l.events[0] = Event{ID: marker, Type: EventTruncated, LimitBytes: MaxOutputBytes}
	l.truncated = true
	l.indexed = 0
	l.stdout.Reset()
	l.stderr.Reset()
}

func (l *eventLog) resumeOutput(kind EventType, offset int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.complete {
		return
	}
	if !l.keepTail {
		if !l.truncated {
			l.truncateLocked()
		}
		return
	}
	last := len(l.events) - 1
	for last >= 0 && l.events[last].Type != kind {
		last--
	}
	if last >= 0 {
		for i := range last + 1 {
			l.dropOutputLocked(i)
		}
		l.markPrefixLocked(last + 1)
	} else if !l.truncated {
		l.appendLocked(Event{Type: EventTruncated, LimitBytes: MaxOutputBytes})
		l.truncated = true
	}
	if kind == EventStdout {
		l.stdoutOffset = offset
	} else {
		l.stderrOffset = offset
	}
	l.notifyLocked()
}

func (l *eventLog) appendOutputLocked(kind EventType, data []byte) {
	if l.outputEvents == 0 && l.onFirstOutput != nil {
		l.onFirstOutput()
	}
	l.appendLocked(Event{Type: kind, Data: append([]byte(nil), data...)})
	l.outputEvents++
	l.retained += len(data)
}

func runeBoundary(chunk []byte, limit int) int {
	if limit >= len(chunk) {
		return limit
	}
	for i := limit; i > 0; i-- {
		if utf8.RuneStart(chunk[i]) {
			return i
		}
	}
	return 0
}

func incompleteRuneTail(chunk []byte) int {
	for i := 1; i < utf8.UTFMax && i <= len(chunk); i++ {
		start := len(chunk) - i
		if !utf8.RuneStart(chunk[start]) {
			continue
		}
		if utf8.FullRune(chunk[start:]) {
			return 0
		}
		return i
	}
	return 0
}

func (l *eventLog) truncateLocked() {
	l.truncated = true
	l.appendLocked(Event{Type: EventTruncated, LimitBytes: MaxOutputBytes})
	l.outputEvents = MaxOutputEvents
}

func (l *eventLog) appendLocked(ev Event) {
	l.nextID++
	ev.ID = l.nextID
	l.events = append(l.events, ev)
	l.notifyLocked()
}

func (l *eventLog) eventsAfterLocked(after uint64) ([]Event, bool) {
	i := len(l.events)
	if i > 0 && after < l.nextID {
		first := l.events[0].ID
		if after < first {
			i = 0
		} else {
			i = int(after - first + 1)
		}
	}
	events := append([]Event(nil), l.events[i:]...)
	if n := len(events); n > 0 && events[n-1].Type == EventComplete && events[n-1].Execution != nil {
		stdout, stderr, truncated := l.outputLocked()
		ex := *events[n-1].Execution
		ex.Stdout, ex.Stderr, ex.OutputTruncated = stdout, stderr, truncated
		events[n-1].Execution = &ex
	}
	return events, l.complete && i+len(events) >= len(l.events)
}

func (l *eventLog) wait(ctx context.Context, after uint64) ([]Event, bool, error) {
	for {
		l.mu.Lock()
		events, done := l.eventsAfterLocked(after)
		ch := l.changed
		l.mu.Unlock()

		if len(events) > 0 || done {
			return events, done, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-ch:
		}
	}
}

func (l *eventLog) truncatedOutput() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.truncated
}

func (l *eventLog) retainedBytes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retained
}

func (l *eventLog) output() (stdout, stderr string, truncated bool, stdoutOffset, stderrOffset int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	stdout, stderr, truncated = l.outputLocked()
	return stdout, stderr, truncated, l.stdoutOffset, l.stderrOffset
}

func (l *eventLog) outputLocked() (stdout, stderr string, truncated bool) {
	for _, ev := range l.events[l.indexed:] {
		switch ev.Type {
		case EventStdout:
			l.stdout.Write(ev.Data)
		case EventStderr:
			l.stderr.Write(ev.Data)
		}
	}
	l.indexed = len(l.events)
	return l.stdout.String(), l.stderr.String(), l.truncated
}

func (l *eventLog) lastEventID() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextID
}
