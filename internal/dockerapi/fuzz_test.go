package dockerapi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func FuzzDemux(f *testing.F) {
	f.Add(multiFrame(byte(streamOut), "hello ", byte(streamErr), "warn\n", byte(streamOut), "world\n"))
	f.Add(multiFrame(byte(streamStdin), "typed at a prompt"))
	f.Add(multiFrame(byte(7), "future stream"))
	f.Add(frame(streamOut, "0123456789")[:12])
	f.Add([]byte{1, 0, 0})
	f.Add([]byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{1, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		wantOut, wantErr, wantIncomplete := expectedDemux(data)

		var out, errBuf bytes.Buffer
		err := Demux(bytes.NewReader(data), &out, &errBuf)

		if got := out.String(); got != wantOut {
			t.Fatalf("stdout = %q, want %q", got, wantOut)
		}
		if got := errBuf.String(); got != wantErr {
			t.Fatalf("stderr = %q, want %q", got, wantErr)
		}
		if gotIncomplete := err != nil; gotIncomplete != wantIncomplete {
			t.Fatalf("Demux error = %v, want incomplete = %v for %q", err, wantIncomplete, data)
		}
	})
}

func expectedDemux(data []byte) (stdout, stderr string, incomplete bool) {
	var out, errBuf bytes.Buffer
	for len(data) > 0 {
		if len(data) < frameHeader {
			return out.String(), errBuf.String(), true
		}
		stream := data[0]
		size := uint64(binary.BigEndian.Uint32(data[4:8]))
		body := data[frameHeader:]
		if uint64(len(body)) < size {
			appendChunk(stream, body, &out, &errBuf)
			return out.String(), errBuf.String(), true
		}
		appendChunk(stream, body[:size], &out, &errBuf)
		data = body[size:]
	}
	return out.String(), errBuf.String(), false
}

func multiFrame(parts ...any) []byte {
	var b bytes.Buffer
	for i := 0; i+1 < len(parts); i += 2 {
		stream, ok := parts[i].(byte)
		if !ok {
			panic("multiFrame: stream type must be a byte")
		}
		payload, ok := parts[i+1].(string)
		if !ok {
			panic("multiFrame: payload must be a string")
		}
		b.Write(frame(stream, payload))
	}
	return b.Bytes()
}

func appendChunk(stream byte, chunk []byte, out, errBuf *bytes.Buffer) {
	switch stream {
	case streamOut:
		out.Write(chunk)
	case streamErr:
		errBuf.Write(chunk)
	}
}
