package api

import (
	"path/filepath"
	"strings"

	"github.com/gofiber/fiber"

	addonpkg "github.com/yundera/stremio-torrent-bridge/internal/addon"
	"github.com/yundera/stremio-torrent-bridge/internal/proxy"
	"github.com/yundera/stremio-torrent-bridge/internal/ui"
)

// AddonRouter is the interface satisfied by a go-stremio addon for registering
// custom HTTP endpoints. Using an interface keeps the API layer testable
// without importing the full stremio dependency.
type AddonRouter interface {
	AddEndpoint(method, path string, handler func(*fiber.Ctx))
}

// RegisterRoutes wires all API, wrap, stream, and UI routes onto the given
// addon router.
//
// Parameters:
//   - router: the go-stremio addon instance (has AddEndpoint method)
//   - h: the management API handlers
//   - w: the Stremio addon wrapper (manifest rewrite, stream interception)
//   - sp: the video stream proxy
func RegisterRoutes(router AddonRouter, h *Handlers, w *addonpkg.Wrapper, sp *proxy.StreamProxy) {
	// --- Management API routes -----------------------------------------------

	router.AddEndpoint("POST", "/api/addons", h.HandleAddAddon)
	router.AddEndpoint("GET", "/api/addons", h.HandleListAddons)
	router.AddEndpoint("DELETE", "/api/addons/:id", h.HandleRemoveAddon)
	router.AddEndpoint("GET", "/api/config", h.HandleGetConfig)
	router.AddEndpoint("PUT", "/api/config", h.HandleUpdateConfig)

	// --- Cache management routes ---------------------------------------------

	router.AddEndpoint("GET", "/api/cache/stats", h.HandleGetCacheStats)
	router.AddEndpoint("POST", "/api/cache/cleanup", h.HandleCacheCleanup)
	router.AddEndpoint("DELETE", "/api/cache/torrents/:hash", h.HandleRemoveTorrent)

	// --- Stremio wrap routes (addon protocol) --------------------------------

	router.AddEndpoint("GET", "/wrap/:wrapId/manifest.json", w.HandleManifest)
	router.AddEndpoint("GET", "/wrap/:wrapId/catalog/:type/:catalogId.json", w.HandleCatalog)
	router.AddEndpoint("GET", "/wrap/:wrapId/meta/:type/:metaId.json", w.HandleMeta)
	router.AddEndpoint("GET", "/wrap/:wrapId/stream/:type/:streamId.json", w.HandleStream)

	// --- Stream proxy route --------------------------------------------------

	router.AddEndpoint("GET", "/stream/:infoHash/:fileIndex", sp.HandleStream)

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
