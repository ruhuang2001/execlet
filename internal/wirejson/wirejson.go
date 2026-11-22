// Package wirejson pins the existing /v1 and Docker JSON wire conventions on
// the Go v2 codec. Policy files and journals have separate, strict contracts.
package wirejson

import (
	legacy "encoding/json"
	"encoding/json/v2"
	"io"
)

// Marshal preserves nils, legacy omitempty, HTML escaping and invalid-UTF-8 replacement.
func Marshal(v any) ([]byte, error) { return json.Marshal(v, legacy.DefaultOptionsV1()) }

// Unmarshal accepts additive fields and preserves existing field-name matching.
func Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v, legacy.DefaultOptionsV1()) }

// Read decodes exactly one complete wire value.
func Read(r io.Reader, v any) error { return json.UnmarshalRead(r, v, legacy.DefaultOptionsV1()) }
