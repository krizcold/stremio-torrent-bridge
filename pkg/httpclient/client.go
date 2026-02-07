package httpclient

import (
	"net/http"
	"time"
)

// New creates an HTTP client with sensible defaults for API calls
func New() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// NewStreaming creates an HTTP client for streaming (no timeout - movies can be hours)
func NewStreaming() *http.Client {
	return &http.Client{
		Timeout: 0, // No timeout for streaming
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}
