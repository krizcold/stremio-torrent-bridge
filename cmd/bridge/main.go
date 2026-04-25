package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gofiber/fiber"

	"github.com/krizcold/stremio-torrent-bridge/internal/addon"
	"github.com/krizcold/stremio-torrent-bridge/internal/api"
	"github.com/krizcold/stremio-torrent-bridge/internal/cache"
	"github.com/krizcold/stremio-torrent-bridge/internal/config"
	"github.com/krizcold/stremio-torrent-bridge/internal/engine"
	"github.com/krizcold/stremio-torrent-bridge/internal/proxy"
	"github.com/krizcold/stremio-torrent-bridge/internal/relay"
)

// fiberRouter adapts a *fiber.App to api.AddonRouter.
type fiberRouter struct {
	app *fiber.App
}

func (r *fiberRouter) AddEndpoint(method, path string, handler func(*fiber.Ctx)) {
	switch method {
	case "GET":
		r.app.Get(path, handler)
	case "POST":
		r.app.Post(path, handler)
	case "PUT":
		r.app.Put(path, handler)
	case "PATCH":
		r.app.Patch(path, handler)
	case "DELETE":
		r.app.Delete(path, handler)
	}
}

func (r *fiberRouter) AddMiddleware(path string, middleware func(*fiber.Ctx)) {
	r.app.Use(path, middleware)
}

func main() {
	cfg := config.Load()
	cfg.LogSummary()

	torrserverAdapter := engine.NewTorrServerAdapter(cfg.TorrServerURL, cfg.TorrServerUsername, cfg.TorrServerPassword)
	rqbitAdapter := engine.NewRqbitAdapter(cfg.RqbitURL, cfg.RqbitUsername, cfg.RqbitPassword)
	qbitAdapter := engine.NewQBittorrentAdapter(cfg.QBittorrentURL, cfg.QBitDownloadPath, cfg.QBitUsername, cfg.QBitPassword)

	multi := engine.NewMultiEngine(torrserverAdapter, rqbitAdapter, qbitAdapter)
	if err := multi.SetActive(cfg.DefaultEngine); err != nil {
		fmt.Printf("Unknown engine %q, falling back to qbittorrent\n", cfg.DefaultEngine)
		_ = multi.SetActive("qbittorrent")
		cfg.DefaultEngine = "qbittorrent"
	}
	var eng engine.Engine = multi
	fmt.Printf("Using engine: %s\n", multi.GetActive())

	cacheManager := cache.NewCacheManager(eng, cfg)

	store, err := addon.NewAddonStore(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create addon store: %v\n", err)
		os.Exit(1)
	}

	relayServer := relay.NewServer()
	wrapper := addon.NewWrapper(store, cfg, eng, relayServer)
	streamProxy := proxy.NewStreamProxy(eng, cacheManager)
	handlers := api.NewHandlers(store, cfg, eng, multi, cacheManager, wrapper, relayServer)

	// We previously used go-stremio's NewAddon to build the Fiber app, but
	// v0.6.0 hard-codes WriteTimeout: 9*time.Second on the underlying server,
	// which severs any video stream that takes longer than 9s — i.e. all of
	// them. Building Fiber ourselves with WriteTimeout: 0 lets long streams
	// run to completion.
	app := fiber.New(&fiber.Settings{
		DisableStartupMessage: true,
		BodyLimit:             0,
		ReadTimeout:           0,
		WriteTimeout:          0,
		IdleTimeout:           0,
	})

	app.Use(func(c *fiber.Ctx) {
		c.Set("Access-Control-Allow-Origin", "*")
		c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Set("Access-Control-Allow-Headers", "Content-Type, Range, If-Range, If-None-Match")
		if c.Method() == "OPTIONS" {
			c.Status(204)
			return
		}
		c.Next()
	})

	bridgeManifest := map[string]interface{}{
		"id":          "com.yundera.torrent-bridge",
		"name":        "Torrent Bridge",
		"description": "Wraps Stremio addons for full TCP/UDP peer connectivity",
		"version":     "0.1.0",
		"logo":        "https://cdn.jsdelivr.net/gh/krizcold/stremio-torrent-bridge@main/assets/icon.png",
		"types":       []string{"movie", "series"},
		"catalogs":    []interface{}{},
		"resources": []map[string]interface{}{
			{"name": "stream", "types": []string{"movie", "series"}},
		},
	}
	manifestJSON, err := json.Marshal(bridgeManifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal bridge manifest: %v\n", err)
		os.Exit(1)
	}
	app.Get("/manifest.json", func(c *fiber.Ctx) {
		c.Set("Content-Type", "application/json")
		c.Send(manifestJSON)
	})

	router := &fiberRouter{app: app}
	api.RegisterRoutes(router, handlers, wrapper, streamProxy, relayServer)

	cacheManager.Start()
	defer cacheManager.Stop()

	listenAddr := fmt.Sprintf("%s:%d", cfg.BindAddr, cfg.Port)
	fmt.Printf("Torrent Bridge starting on %s\n", listenAddr)
	if err := app.Listen(listenAddr); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
