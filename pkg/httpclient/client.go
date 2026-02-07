package httpclient

import (
	"net/http"
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
