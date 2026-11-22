package dockerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ruhuang2001/tiny-sandbox/internal/wirejson"
)

const maxErrorBody = 64 << 10

type Client struct {
	base    string
	http    *http.Client
	version string
}

func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), http: hc}
}

func Dial(ctx context.Context, socketPath string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:       16,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	c := &Client{base: "http://docker", http: &http.Client{Transport: tr}}
	if err := c.Negotiate(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/_ping", nil)
	if err != nil {
		return err
	}
	probe := *c.http
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := probe.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dockerapi: ping status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 3))
	if err != nil {
		return err
	}
	if string(body) != "OK" {
		return errors.New("dockerapi: invalid ping response")
	}
	return nil
}

func (c *Client) Version() string { return strings.TrimPrefix(c.version, "/v") }

func (c *Client) Negotiate(ctx context.Context) error {
	var v struct {
		APIVersion string `json:"ApiVersion"`
		Version    string `json:"Version"`
	}
	if err := c.getJSON(ctx, "/version", &v); err != nil {
		return fmt.Errorf("dockerapi: negotiate: %w", err)
	}
	if v.APIVersion == "" {
		return errors.New("dockerapi: negotiate: daemon reported no API version")
	}
	c.version = "/v" + v.APIVersion
	return nil
}

func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.postJSON(ctx, "/containers/create", q, cfg, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("dockerapi: create returned no container id")
	}
	return out.ID, nil
}

func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil)
}

func (c *Client) ContainerRemove(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}, "v": {"1"}}
	return c.send(ctx, http.MethodDelete, "/containers/"+id, q, nil)
}

func (c *Client) ContainerRemoveStopped(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/containers/"+id, url.Values{"v": {"1"}}, nil)
}

func (c *Client) ContainerInspect(ctx context.Context, id string) (ContainerInspect, error) {
	var out ContainerInspect
	if err := c.getJSON(ctx, "/containers/"+id+"/json", &out); err != nil {
		return ContainerInspect{}, err
	}
	return out, nil
}

func (c *Client) ContainerPause(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/pause", nil, nil)
}

func (c *Client) ContainerUnpause(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/unpause", nil, nil)
}

func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	q := url.Values{}
	if signal != "" {
		q.Set("signal", signal)
	}
	return c.send(ctx, http.MethodPost, "/containers/"+id+"/kill", q, nil)
}

func (c *Client) ContainerList(ctx context.Context, all bool, filters map[string][]string) ([]Container, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	if len(filters) > 0 {
		raw, err := wirejson.Marshal(filters)
		if err != nil {
			return nil, fmt.Errorf("dockerapi: encode filters: %w", err)
		}
		q.Set("filters", string(raw))
	}
	var out []Container
	if err := c.getJSONQuery(ctx, "/containers/json", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) ExecCreate(ctx context.Context, containerID string, cfg ExecConfig) (string, error) {
	body := cfg
	body.AttachStdout = true
	body.AttachStderr = true
	body.AttachStdin = false
	body.Tty = false
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.postJSON(ctx, "/containers/"+containerID+"/exec", nil, body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("dockerapi: exec create returned no id")
	}
	return out.ID, nil
}

func (c *Client) ExecStart(ctx context.Context, execID string) (io.ReadCloser, error) {
	q := url.Values{"detach": {"false"}}
	body, err := wirejson.Marshal(execStart{Detach: false, Tty: false})
	if err != nil {
		return nil, fmt.Errorf("dockerapi: encode exec start: %w", err)
	}
	req, err := c.request(ctx, http.MethodPost, "/exec/"+execID+"/start", q, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) ExecInspect(ctx context.Context, execID string) (ExecInspect, error) {
	var out ExecInspect
	if err := c.getJSON(ctx, "/exec/"+execID+"/json", &out); err != nil {
		return ExecInspect{}, err
	}
	return out, nil
}

func (c *Client) ArchiveGet(ctx context.Context, containerID, path string) (io.ReadCloser, error) {
	q := url.Values{"path": {path}}
	req, err := c.request(ctx, http.MethodGet, "/containers/"+containerID+"/archive", q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-tar")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) ArchivePut(ctx context.Context, containerID, path string, tarBody []byte) error {
	q := url.Values{"path": {path}}
	req, err := c.request(ctx, http.MethodPut, "/containers/"+containerID+"/archive", q, bytes.NewReader(tarBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *Client) path(p string) string { return c.version + p }

func (c *Client) request(ctx context.Context, method, p string, q url.Values, body io.Reader) (*http.Request, error) {
	u := c.base + c.path(p)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: build request: %w", err)
	}
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, statusError(resp)
}

func (c *Client) send(ctx context.Context, method, p string, q url.Values, body []byte) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := c.request(ctx, method, p, q, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *Client) getJSON(ctx context.Context, p string, out any) error {
	return c.getJSONQuery(ctx, p, nil, out)
}

func (c *Client) getJSONQuery(ctx context.Context, p string, q url.Values, out any) error {
	req, err := c.request(ctx, http.MethodGet, p, q, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := wirejson.Read(resp.Body, out); err != nil {
		return fmt.Errorf("dockerapi: decode %s: %w", p, err)
	}
	return nil
}

func (c *Client) postJSON(ctx context.Context, p string, q url.Values, in, out any) error {
	body, err := wirejson.Marshal(in)
	if err != nil {
		return fmt.Errorf("dockerapi: encode %s: %w", p, err)
	}
	req, err := c.request(ctx, http.MethodPost, p, q, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := wirejson.Read(resp.Body, out); err != nil {
		return fmt.Errorf("dockerapi: decode %s: %w", p, err)
	}
	return nil
}
