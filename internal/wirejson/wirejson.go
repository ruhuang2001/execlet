package wirejson

import (
	legacy "encoding/json"
	"encoding/json/v2"
	"io"
)

func Marshal(v any) ([]byte, error) { return json.Marshal(v, legacy.DefaultOptionsV1()) }

func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v, legacy.DefaultOptionsV1()) }

func Read(r io.Reader, v any) error { return json.UnmarshalRead(r, v, legacy.DefaultOptionsV1()) }
