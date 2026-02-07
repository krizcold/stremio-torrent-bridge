package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gofiber/fiber"

	"github.com/krizcold/stremio-torrent-bridge/internal/cache"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
)

// param reads a named value from Fiber context, checking Locals first (set by
// middleware routing) then falling back to Params (set by Fiber route params).
func param(c *fiber.Ctx, key string) string {
	if v, ok := c.Locals(key).(string); ok && v != "" {
		return v
	}
	return c.Params(key)
}

// hopByHopHeaders are headers that must not be forwarded from the upstream
// engine response to the client. These are connection-scoped and meaningless
// for the end-to-end stream delivery.
var hopByHopHeaders = map[string]struct{}{
	"Connection":        {},
	"Keep-Alive":        {},
	"Transfer-Encoding": {},
	"Te":                {},
	"Trailer":           {},
	"Upgrade":           {},
}

// StreamProxy handles proxying video streams from the torrent engine to the
// HTTP client. It supports Range requests for seeking within video players.
type StreamProxy struct {
	engine       engine.Engine
	cacheManager *cache.CacheManager // may be nil
}

// NewStreamProxy creates a new StreamProxy backed by the given engine.
// The optional cacheManager records access times for LRU eviction.
func NewStreamProxy(eng engine.Engine, cm *cache.CacheManager) *StreamProxy {
	return &StreamProxy{engine: eng, cacheManager: cm}
}

// HandleStream is the Fiber v1 handler for GET /stream/:infoHash/:fileIndex.
// It proxies the video stream from the torrent engine to the client with zero
// buffering, preserving Range request semantics for seek support.
func (sp *StreamProxy) HandleStream(c *fiber.Ctx) {
	infoHash := param(c, "infoHash")
	if infoHash == "" {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"missing infoHash path parameter"}`)
		return
	}

	fileIndex := 0
	if fi := param(c, "fileIndex"); fi != "" {
		parsed, err := strconv.Atoi(fi)
		if err != nil {
			c.Status(http.StatusBadRequest)
			c.Set("Content-Type", "application/json")
			c.SendString(`{"error":"fileIndex must be an integer"}`)
			return
		}
		fileIndex = parsed
	}

	// Build a standard *http.Request so the engine adapter can read Range
	// and other relevant headers for partial content support.
	reqURL := fmt.Sprintf("http://localhost/stream/%s/%d", infoHash, fileIndex)
	httpReq, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		c.Status(http.StatusInternalServerError)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"failed to construct upstream request"}`)
		return
	}

	// Copy headers that the engine adapter needs for Range support and
	// conditional requests.
	forwardHeaders := []string{"Range", "If-Range", "If-None-Match", "Accept"}
	for _, h := range forwardHeaders {
		if v := c.Get(h); v != "" {
			httpReq.Header.Set(h, v)
		}
	}

	// Use context.Background() because streaming has no timeout -- a movie
	// can play for hours and the connection must stay open.
	resp, err := sp.engine.StreamFile(context.Background(), infoHash, fileIndex, httpReq)
	if err != nil {
		c.Status(http.StatusBadGateway)
		c.Set("Content-Type", "application/json")
		errJSON, _ := json.Marshal(map[string]string{
			"error": fmt.Sprintf("engine stream failed: %v", err),
		})
		c.SendString(string(errJSON))
		return
	}

	// Record the torrent access for LRU cache management.
	if sp.cacheManager != nil {
		go func() {
			info, err := sp.engine.GetTorrent(context.Background(), infoHash)
			if err == nil && info != nil {
				var totalSize int64
				for _, f := range info.Files {
					totalSize += f.Size
				}
				sp.cacheManager.RecordAccess(infoHash, info.Name, totalSize)
			} else {
				// Still record the access even without full info.
				sp.cacheManager.RecordAccess(infoHash, "", 0)
			}
		}()
	}

	// Set the response status code. This correctly handles both 200 OK and
	// 206 Partial Content from Range requests.
	c.Status(resp.StatusCode)

	// Copy all response headers from the engine, skipping hop-by-hop headers
	// that are connection-scoped and must not be forwarded.
	for key, values := range resp.Header {
		if _, skip := hopByHopHeaders[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, v := range values {
			c.Set(key, v)
		}
	}

	// Stream the body with zero buffering. SetBodyStream hands the reader
	// directly to fasthttp which reads from it in chunks as the client
	// consumes data. fasthttp will close the reader when streaming completes
	// or the connection drops.
	contentLength := int(resp.ContentLength)
	if resp.ContentLength < 0 {
		contentLength = -1
	}
	c.Fasthttp.Response.SetBodyStream(resp.Body, contentLength)
}
