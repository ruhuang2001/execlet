package dockerapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	frameHeader = 8
	streamStdin = 0
	streamOut   = 1
	streamErr   = 2
)

func Demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [frameHeader]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("dockerapi: read frame header: %w", err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		var w io.Writer
		switch hdr[0] {
		case streamOut:
			w = stdout
		case streamErr:
			w = stderr
		case streamStdin:
		default:
		}
		if w == nil {
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return fmt.Errorf("dockerapi: discard %d-byte frame: %w", size, err)
			}
			continue
		}
		if _, err := io.CopyN(w, r, size); err != nil {
			return fmt.Errorf("dockerapi: copy %d-byte frame: %w", size, err)
		}
	}
}
