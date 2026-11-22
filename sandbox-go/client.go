package sandbox

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Domain     string
	APIKey     string
	HTTPClient *http.Client
	MaxRetries int
}

type Client struct {
	domain        string
	apiKey        string
	httpClient    *http.Client
	maxRetries    int
	discoveryGate chan struct{}
	capabilities  []string
}

func NewClient(cfg Config) (*Client, error) {
	domain := cfg.Domain
	if domain == "" {
		domain = os.Getenv("SANDBOX_DOMAIN")
	}
	if domain == "" {
		domain = "http://localhost:8080"
	}
	if !strings.Contains(domain, "://") {
		domain = "http://" + domain
	}
	u, err := url.Parse(domain)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("sandbox: invalid domain %q", cfg.Domain)
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("SANDBOX_API_KEY")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}

	maxRetries := cfg.MaxRetries
	switch {
	case maxRetries == 0:
		maxRetries = 3
	case maxRetries < 0:
		maxRetries = 0
	}

	return &Client{
		domain:        strings.TrimSuffix(u.String(), "/"),
		apiKey:        apiKey,
		httpClient:    httpClient,
		maxRetries:    maxRetries,
		discoveryGate: make(chan struct{}, 1),
	}, nil
}
