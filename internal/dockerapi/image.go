package dockerapi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ruhuang2001/tiny-sandbox/internal/wirejson"
)

func (c *Client) PullImage(ctx context.Context, image string) error {
	repository, tag := splitImage(image)
	q := url.Values{"fromImage": {repository}}
	if tag != "" {
		q.Set("tag", tag)
	}
	req, err := c.request(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("dockerapi: pull %s: %w", image, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return parsePullStream(resp.Body)
}

func splitImage(image string) (repository, tag string) {
	if strings.HasPrefix(image, "sha256:") {
		return image, ""
	}
	if repo, digest, ok := strings.Cut(image, "@"); ok && digest != "" {
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		return repo, digest
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

func parsePullStream(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload struct {
			Error string `json:"error"`
		}
		if err := wirejson.Unmarshal([]byte(line), &payload); err != nil {
			continue
		}
		if payload.Error != "" {
			return fmt.Errorf("dockerapi: pull failed: %s", payload.Error)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("dockerapi: read pull stream: %w", err)
	}
	return nil
}

func IsNoSuchImage(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Status == http.StatusNotFound && strings.Contains(strings.ToLower(se.Message), "no such image")
}
