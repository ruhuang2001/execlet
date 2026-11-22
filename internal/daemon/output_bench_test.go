package daemon

import "testing"

func BenchmarkRetainedSnapshot(b *testing.B) {
	ex := &execution{log: newEventLog()}
	chunk := make([]byte, MaxOutputBytes/MaxOutputEvents)
	for range MaxOutputEvents {
		ex.log.appendOutput(EventStdout, chunk)
	}
	b.ReportAllocs()
	for b.Loop() {
		if ex.snapshot().Stdout == "" {
			b.Fatal("missing output")
		}
	}
}
