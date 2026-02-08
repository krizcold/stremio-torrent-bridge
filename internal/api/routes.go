package api

import (
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber"

	addonpkg "github.com/krizcold/stremio-torrent-bridge/internal/addon"
	"github.com/krizcold/stremio-torrent-bridge/internal/proxy"
	"github.com/krizcold/stremio-torrent-bridge/internal/relay"
	"github.com/krizcold/stremio-torrent-bridge/internal/ui"
)

// AddonRouter is the interface satisfied by a go-stremio addon for registering
// custom HTTP endpoints and middleware. Using an interface keeps the API layer
// testable without importing the full stremio dependency.
type AddonRouter interface {
	AddEndpoint(method, path string, handler func(*fiber.Ctx))
	AddMiddleware(path string, middleware func(*fiber.Ctx))
}

// RegisterRoutes wires all API, wrap, stream, and UI routes onto the given
// addon router.
//
// Parameters:
//   - router: the go-stremio addon instance (has AddEndpoint method)
//   - h: the management API handlers
//   - w: the Stremio addon wrapper (manifest rewrite, stream interception)
//   - sp: the video stream proxy
func RegisterRoutes(router AddonRouter, h *Handlers, w *addonpkg.Wrapper, sp *proxy.StreamProxy, rs *relay.Server) {
	// --- Management API routes -----------------------------------------------

	router.AddEndpoint("POST", "/api/addons", h.HandleAddAddon)
	router.AddEndpoint("GET", "/api/addons", h.HandleListAddons)
	router.AddEndpoint("DELETE", "/api/addons/:id", h.HandleRemoveAddon)
	router.AddEndpoint("PATCH", "/api/addons/:id", h.HandleUpdateAddon)
	router.AddEndpoint("GET", "/api/config", h.HandleGetConfig)
	router.AddEndpoint("PUT", "/api/config", h.HandleUpdateConfig)

	// --- Cache management routes ---------------------------------------------

	router.AddEndpoint("GET", "/api/cache/stats", h.HandleGetCacheStats)
	router.AddEndpoint("POST", "/api/cache/cleanup", h.HandleCacheCleanup)
	router.AddEndpoint("DELETE", "/api/cache/torrents/:hash", h.HandleRemoveTorrent)

	// --- Stremio wrap routes (addon protocol) --------------------------------
	// Registered as middleware so they run BEFORE go-stremio's built-in route
	// handlers, which would otherwise intercept paths containing manifest.json,
	// /stream/, and /catalog/ patterns.

	router.AddMiddleware("/wrap", wrapMiddleware(w))

	// --- Stream proxy route --------------------------------------------------
	// Also registered as middleware to avoid conflict with go-stremio's
	// /stream/:type/:id.json handler.

	router.AddMiddleware("/stream", streamProxyMiddleware(sp))

	// --- Browser Tab Relay routes ---------------------------------------------

	router.AddEndpoint("GET", "/api/relay/pending", rs.HandlePending)
	router.AddEndpoint("POST", "/api/relay/response/:id", rs.HandleResponse)
	router.AddEndpoint("GET", "/api/relay/status", rs.HandleStatus)

	// --- Service Worker routes ------------------------------------------------
	// These must be publicly accessible (no auth hash required).
	// nginx-hash-lock ALLOWED_PATHS includes "sw".

	router.AddEndpoint("GET", "/sw/config.json", h.HandleSWConfig)

	// --- UI routes (embedded static files) -----------------------------------

	router.AddEndpoint("GET", "/ui/*", func(c *fiber.Ctx) {
		path := c.Params("*")
		if path == "" {
			path = "index.html"
		}

		data, err := ui.StaticFiles.ReadFile("static/" + path)
		if err != nil {
			c.Status(404)
			c.SendString("Not found")
			return
		}

		c.Set("Content-Type", contentTypeFromExt(path))
		c.Send(data)
	})

	router.AddEndpoint("GET", "/", func(c *fiber.Ctx) {
		c.Set("Location", "/ui/index.html")
		c.Status(301)
		c.SendString("Redirecting to /ui/index.html")
	})
}

// wrapMiddleware returns a Fiber handler that intercepts requests under /wrap/
// and routes them to the appropriate Wrapper method based on the URL structure.
// It does NOT call c.Next() for matched requests, preventing go-stremio from
// handling them.
func wrapMiddleware(w *addonpkg.Wrapper) func(*fiber.Ctx) {
	return func(c *fiber.Ctx) {
		if c.Method() != "GET" {
			c.Next()
			return
		}

		path := c.Path()

		// Strip the /wrap/ prefix to get: {wrapId}/manifest.json, etc.
		rest := strings.TrimPrefix(path, "/wrap/")
		if rest == path {
			// Didn't start with /wrap/
			c.Next()
			return
		}

		parts := strings.SplitN(rest, "/", 2)
		if len(parts) < 2 {
			c.Next()
			return
		}

		wrapID := parts[0]
		remainder := parts[1] // e.g. "manifest.json", "stream/movie/tt123.json"

		// Inject wrapId as a Fiber local so handlers can read it.
		c.Locals("wrapId", wrapID)

		switch {
		case remainder == "manifest.json":
			w.HandleManifest(c)
		case strings.HasPrefix(remainder, "catalog/"):
			// catalog/{type}/{catalogId}.json
			seg := strings.TrimPrefix(remainder, "catalog/")
			typAndID := strings.SplitN(seg, "/", 2)
			if len(typAndID) == 2 {
				c.Locals("type", typAndID[0])
				c.Locals("catalogId", strings.TrimSuffix(typAndID[1], ".json"))
				w.HandleCatalog(c)
			} else {
				c.Next()
			}
		case strings.HasPrefix(remainder, "meta/"):
			seg := strings.TrimPrefix(remainder, "meta/")
			typAndID := strings.SplitN(seg, "/", 2)
			if len(typAndID) == 2 {
				c.Locals("type", typAndID[0])
				c.Locals("metaId", strings.TrimSuffix(typAndID[1], ".json"))
				w.HandleMeta(c)
			} else {
				c.Next()
			}
		case strings.HasPrefix(remainder, "stream/"):
			seg := strings.TrimPrefix(remainder, "stream/")
			typAndID := strings.SplitN(seg, "/", 2)
			if len(typAndID) == 2 {
				c.Locals("type", typAndID[0])
				c.Locals("streamId", strings.TrimSuffix(typAndID[1], ".json"))
				w.HandleStream(c)
			} else {
				c.Next()
			}
		default:
			c.Next()
		}
	}
}

// streamProxyMiddleware returns a Fiber handler that intercepts requests under
// /stream/ for the video stream proxy. It matches /stream/{infoHash}/{fileIndex}
// (no .json suffix) and prevents go-stremio from catching these.
func streamProxyMiddleware(sp *proxy.StreamProxy) func(*fiber.Ctx) {
	return func(c *fiber.Ctx) {
		if c.Method() != "GET" {
			c.Next()
			return
		}

		path := c.Path()
		rest := strings.TrimPrefix(path, "/stream/")
		if rest == path {
			c.Next()
			return
		}

		// Only match /stream/{infoHash}/{fileIndex} (no .json suffix).
		// Requests ending in .json are go-stremio's stream protocol and
		// should fall through.
		if strings.HasSuffix(path, ".json") {
			c.Next()
			return
		}

		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			c.Next()
			return
		}

		c.Locals("infoHash", parts[0])
		c.Locals("fileIndex", parts[1])
		sp.HandleStream(c)
	}
}

// contentTypeFromExt returns the MIME content type for common static file
// extensions. Falls back to application/octet-stream for unknown types.
func contentTypeFromExt(path string) string {
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "application/javascript"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}
