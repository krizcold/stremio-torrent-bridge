package addon

import "time"

// Valid fetch method values.
const (
	FetchMethodGlobal     = "global"      // Use the global default
	FetchMethodSWFallback = "sw_fallback" // Service Worker with server-side fallback
	FetchMethodTabRelay   = "tab_relay"   // Browser Tab Relay via WebSocket
	FetchMethodSWOnly     = "sw_only"     // Service Worker only, no fallback
	FetchMethodDirect     = "direct"      // Server-side fetch (PCS-IP only)
	FetchMethodProxy      = "proxy"       // Server-side fetch through custom proxy
)

// Valid fetch status values.
const (
	FetchStatusOK      = "ok"
	FetchStatusBlocked = "blocked"
	FetchStatusUnknown = "unknown"
)

// ValidFetchMethods lists all valid per-addon fetch methods (includes "global").
var ValidFetchMethods = map[string]bool{
	FetchMethodGlobal:     true,
	FetchMethodSWFallback: true,
	FetchMethodTabRelay:   true,
	FetchMethodSWOnly:     true,
	FetchMethodDirect:     true,
	FetchMethodProxy:      true,
}

// ValidGlobalFetchMethods lists valid global default methods (excludes "global").
var ValidGlobalFetchMethods = map[string]bool{
	FetchMethodSWFallback: true,
	FetchMethodTabRelay:   true,
	FetchMethodSWOnly:     true,
	FetchMethodDirect:     true,
	FetchMethodProxy:      true,
}

// WrappedAddon represents a wrapped Stremio addon configuration
type WrappedAddon struct {
	ID          string    `json:"id"`
	OriginalURL string    `json:"originalUrl"`
	Name        string    `json:"name"`
	FetchMethod string    `json:"fetchMethod"` // Per-addon fetch method ("global" = use default)
	FetchStatus string    `json:"fetchStatus"` // ok, blocked, unknown
	CreatedAt   time.Time `json:"createdAt"`
}
