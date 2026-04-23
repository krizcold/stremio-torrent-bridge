package httpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const userAgent = "StremioTorrentBridge/1.0"

// uaTransport wraps an http.RoundTripper and sets a User-Agent header on
// every outgoing request. Some upstream services (e.g. Torrentio) reject
// Go's default "Go-http-client/1.1" user agent.
type uaTransport struct {
	base http.RoundTripper
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	return t.base.RoundTrip(req)
}

// New creates an HTTP client with sensible defaults for API calls
func New() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &uaTransport{
			base: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// NewStreaming creates an HTTP client for streaming (no timeout - movies can be hours)
func NewStreaming() *http.Client {
	return &http.Client{
		Timeout: 0, // No timeout for streaming
		Transport: &uaTransport{
			base: &http.Transport{
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// NewProxied creates an API client that routes requests through the given
// proxy URL. Supports http://, https://, and socks5:// schemes natively via
// Go's http.ProxyURL. Returns an error if proxyURL is empty or unparseable.
func NewProxied(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return nil, fmt.Errorf("proxy URL is empty")
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("proxy URL missing scheme or host: %q", proxyURL)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &uaTransport{
			base: &http.Transport{
				Proxy:               http.ProxyURL(parsed),
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}, nil
}
