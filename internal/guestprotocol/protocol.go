package protocol

import (
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"io"
	"time"
)

const Version = 1
const MaxFrame = 1 << 20
const Socket = "/run/execlet/agent.sock"
const Binary = "/.execlet/guest"

type Identity struct {
	UID    uint32
	GID    uint32
	Groups []uint32
	Dir    string
	Home   string
}
type Request struct {
	WaitMS     int64 `json:",omitzero"`
	OutputTail bool  `json:",omitzero"`
	ID         string
	Op         string
	Path       string
	After      string
	Limit      int
	Command    []string
	Env        []string
	Dir        string
	Stdin      string
	TimeoutMS  int64
	Offset     int64
	ErrOffset  int64
	Data       []byte
	Rows, Cols uint16
}
type Result struct {
	ErrNext  int64 `json:",omitzero"`
	Version  int
	PID      int
	Done     bool
	ExitCode int
	Reason   string
	Stdout   []byte
	Stderr   []byte
	Next     int64
	Signaled bool
}
type FileEntry struct {
	Name          string    `json:"name"`
	Size          int64     `json:"size"`
	Mode          int64     `json:"mode"`
	Mtime         time.Time `json:"mtime"`
	IsDir         bool      `json:"is_dir"`
	Type          string    `json:"type"`
	UID           int       `json:"uid"`
	GID           int       `json:"gid"`
	Owner         string    `json:"owner,omitempty"`
	Group         string    `json:"group,omitempty"`
	SymlinkTarget string    `json:"symlink_target,omitempty"`
}
type ListPage[T any] struct {
	Entries []T  `json:"entries"`
	More    bool `json:"more,omitzero"`
}

var ErrFrameTooLarge = errors.New("guest frame exceeds 1 MiB")

func ResponseType(kind byte) byte {
	if kind == 'K' {
		return 'c'
	}
	return kind + 32
}

const (
	CodeInsufficientStorage = "insufficient_storage"
	CodeReadOnlyFilesystem  = "read_only_filesystem"
)

type Error struct {
	Code, Message string
	ArchiveMember string `json:",omitempty"`
}

func (e *Error) Error() string { return e.Message }

func Write(w io.Writer, kind byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > MaxFrame {
		return ErrFrameTooLarge
	}
	frame := make([]byte, 5, len(data)+5)
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:], uint32(len(data)))
	_, err = w.Write(append(frame, data...))
	return err
}
func Read(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrameTooLarge
	}
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return head[0], data, err
}
