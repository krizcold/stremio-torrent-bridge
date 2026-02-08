package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber"
)

// FetchRequest is sent to the browser for it to fetch on our behalf.
type FetchRequest struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// FetchResponse is sent back from the browser with the fetched data.
type FetchResponse struct {
	ID         string `json:"id"`
	StatusCode int    `json:"statusCode"`
	Body       string `json:"body"`
	Error      string `json:"error,omitempty"`
}

// Server implements the Browser Tab Relay using HTTP long-polling.
// The bridge puts fetch requests into a queue; the browser long-polls to pick
// them up, fetches the URL using its residential IP, and posts the response
// back.
type Server struct {
	mu       sync.Mutex
	pending  []*pendingEntry          // queue of requests waiting for a browser
	channels map[string]chan *FetchResponse // requestID -> response channel

	nextID    atomic.Int64
	lastPoll  atomic.Int64 // unix timestamp of last browser poll
}

type pendingEntry struct {
	req       *FetchRequest
	createdAt time.Time
}

// NewServer creates a new relay server.
func NewServer() *Server {
	return &Server{
		channels: make(map[string]chan *FetchResponse),
	}
}

// Connected returns true if a browser has polled within the last 10 seconds.
func (s *Server) Connected() bool {
	last := s.lastPoll.Load()
	if last == 0 {
		return false
	}
	return time.Since(time.Unix(last, 0)) < 10*time.Second
}

// Fetch sends a URL to the connected browser for fetching and waits for the
// response. Returns the response body bytes, or an error if the browser is not
// connected or the request times out.
func (s *Server) Fetch(rawURL string, timeout time.Duration) ([]byte, int, error) {
	if !s.Connected() {
		return nil, 0, fmt.Errorf("relay: no browser connected")
	}

	reqID := fmt.Sprintf("r%d", s.nextID.Add(1))

	responseCh := make(chan *FetchResponse, 1)

	s.mu.Lock()
	s.channels[reqID] = responseCh
	s.pending = append(s.pending, &pendingEntry{
		req:       &FetchRequest{ID: reqID, URL: rawURL},
		createdAt: time.Now(),
	})
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.channels, reqID)
		s.mu.Unlock()
	}()

	select {
	case resp := <-responseCh:
		if resp.Error != "" {
			return nil, 0, fmt.Errorf("relay: browser fetch failed: %s", resp.Error)
		}
		return []byte(resp.Body), resp.StatusCode, nil
	case <-time.After(timeout):
		return nil, 0, fmt.Errorf("relay: timeout waiting for browser response")
	}
}

// HandlePending is the long-poll endpoint: GET /api/relay/pending.
// The browser calls this repeatedly. It blocks for up to 25 seconds waiting
// for a pending request. Returns 204 No Content if nothing is pending.
func (s *Server) HandlePending(c *fiber.Ctx) {
	s.lastPoll.Store(time.Now().Unix())

	// Check for an immediately available request.
	if req := s.dequeue(); req != nil {
		out, _ := json.Marshal(req)
		c.Set("Content-Type", "application/json")
		c.Send(out)
		return
	}

	// Long-poll: wait up to 25 seconds for a request to appear.
	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			c.Status(http.StatusNoContent)
			return
		case <-ticker.C:
			s.lastPoll.Store(time.Now().Unix())
			if req := s.dequeue(); req != nil {
				out, _ := json.Marshal(req)
				c.Set("Content-Type", "application/json")
				c.Send(out)
				return
			}
		}
	}
}

// HandleResponse is the callback endpoint: POST /api/relay/response/:id.
// The browser posts the fetched data here after completing a relay request.
func (s *Server) HandleResponse(c *fiber.Ctx) {
	reqID := c.Params("id")
	if reqID == "" {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"missing request id"}`)
		return
	}

	var resp FetchResponse
	if err := json.Unmarshal([]byte(c.Body()), &resp); err != nil {
		c.Status(http.StatusBadRequest)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"invalid JSON body"}`)
		return
	}
	resp.ID = reqID

	s.mu.Lock()
	ch, ok := s.channels[reqID]
	s.mu.Unlock()

	if !ok {
		// Request already timed out or was cancelled.
		c.Status(http.StatusGone)
		c.Set("Content-Type", "application/json")
		c.SendString(`{"error":"request expired or unknown"}`)
		return
	}

	// Non-blocking send — if nobody is listening, the request already timed out.
	select {
	case ch <- &resp:
	default:
	}

	c.Set("Content-Type", "application/json")
	c.SendString(`{"success":true}`)
}

// HandleStatus returns the relay connection status: GET /api/relay/status.
func (s *Server) HandleStatus(c *fiber.Ctx) {
	connected := s.Connected()
	status := "disconnected"
	if connected {
		status = "connected"
	}

	out, _ := json.Marshal(map[string]interface{}{
		"connected": connected,
		"status":    status,
	})
	c.Set("Content-Type", "application/json")
	c.Send(out)
}

// dequeue removes and returns the oldest pending request, or nil if empty.
// It also cleans up stale requests older than 60 seconds.
func (s *Server) dequeue() *FetchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// Remove stale entries.
	fresh := s.pending[:0]
	for _, entry := range s.pending {
		if now.Sub(entry.createdAt) < 60*time.Second {
			fresh = append(fresh, entry)
		}
	}
	s.pending = fresh

	if len(s.pending) == 0 {
		return nil
	}

	// Pop the oldest entry.
	entry := s.pending[0]
	s.pending = s.pending[1:]
	return entry.req
}
