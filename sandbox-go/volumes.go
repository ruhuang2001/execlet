package sandbox

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
)

const (
	volumeCapability = "volumes.v1"
	volumesPath      = "/v1/volumes"
	volumeMiB        = 1 << 20
	minVolumeBytes   = 16 * volumeMiB
	maxVolumeBytes   = 1 << 40
)

type Volume struct {
	ID        string `json:"id"`
	SizeBytes int64  `json:"size_bytes"`
	CreatedAt Time   `json:"created_at"`
}

func (v *Volume) UnmarshalJSON(data []byte) error {
	type wireVolume Volume
	var value wireVolume
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value.ID == "" || value.SizeBytes <= 0 || value.CreatedAt.IsZero() {
		return errors.New("sandbox: invalid volume metadata")
	}
	*v = Volume(value)
	return nil
}

type CreateVolumeRequest struct {
	SizeBytes int64 `json:"size_bytes"`
}

type VolumeMount struct {
	VolumeID string `json:"volume_id"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitzero"`
}

func (c *Client) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	if req.SizeBytes < minVolumeBytes || req.SizeBytes > maxVolumeBytes || req.SizeBytes%volumeMiB != 0 {
		return nil, &Error{Code: "invalid_request", Message: "sandbox: size_bytes must be a whole MiB between 16 MiB and 1 TiB"}
	}
	return c.volumeOp(ctx, request{method: http.MethodPost, path: volumesPath, body: req})
}

func (c *Client) GetVolume(ctx context.Context, volumeID string) (*Volume, error) {
	path, err := volumePath(volumeID)
	if err != nil {
		return nil, err
	}
	return c.volumeOp(ctx, request{method: http.MethodGet, path: path, repeatable: true})
}

func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	if err := c.RequireCapability(ctx, volumeCapability); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, path: volumesPath, repeatable: true})
	if err != nil {
		return nil, err
	}
	var list struct {
		Volumes []Volume `json:"volumes"`
	}
	if err := decode(resp, &list); err != nil {
		return nil, err
	}
	if list.Volumes == nil {
		return nil, &Error{StatusCode: resp.status, Code: "internal", Message: "sandbox: missing volume list"}
	}
	return list.Volumes, nil
}

func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	path, err := volumePath(volumeID)
	if err != nil {
		return err
	}
	if err := c.RequireCapability(ctx, volumeCapability); err != nil {
		return err
	}
	_, err = c.do(ctx, request{method: http.MethodDelete, path: path, retryUnavailableOnly: true})
	return err
}

func (c *Client) volumeOp(ctx context.Context, req request) (*Volume, error) {
	if err := c.RequireCapability(ctx, volumeCapability); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	var volume Volume
	if err := decode(resp, &volume); err != nil {
		return nil, err
	}
	return &volume, nil
}

func volumePath(id string) (string, error) {
	if id == "" || id == "." || id == ".." {
		return "", &Error{Code: "invalid_request", Message: "sandbox: invalid volume_id"}
	}
	return volumesPath + "/" + url.PathEscape(id), nil
}
