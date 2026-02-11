package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all configuration for the stremio-torrent-bridge
type Config struct {
	// Server
	BindAddr    string // env: BIND_ADDR, default: "0.0.0.0"
	Port        int    // env: PORT, default: 8080
	ExternalURL string // env: BRIDGE_EXTERNAL_URL, default: "" (will fallback to Host header)

	// Engine selection
	DefaultEngine string // env: TORRENT_ENGINE, default: "torrserver"

	// Engine URLs
	TorrServerURL      string // env: TORRSERVER_URL, default: "http://torrserver:8090"
	TorrServerUsername  string // env: TORRSERVER_USERNAME, default: "" (no auth)
	TorrServerPassword  string // env: TORRSERVER_PASSWORD, default: ""
	RqbitURL           string // env: RQBIT_URL, default: "http://rqbit:3030"
	RqbitUsername       string // env: RQBIT_USERNAME, default: "" (no auth)
	RqbitPassword       string // env: RQBIT_PASSWORD, default: ""
	QBittorrentURL     string // env: QBITTORRENT_URL, default: "http://qbittorrent:8080"
	QBitDownloadPath   string // env: QBITTORRENT_DOWNLOAD_PATH, default: "/downloads"
	QBitUsername       string // env: QBITTORRENT_USERNAME, default: "admin"
	QBitPassword       string // env: QBITTORRENT_PASSWORD, default: "adminadmin"

	// Fetch proxy
	DefaultFetchMethod string // env: DEFAULT_FETCH_METHOD, default: "sw_fallback"
	ProxyURL           string // env: PROXY_URL, default: "" (for custom proxy fetch method)

	// Cache
	CacheSizeGB     int // env: CACHE_SIZE_GB, default: 60
	CacheMaxAgeDays int // env: CACHE_MAX_AGE_DAYS, default: 7

	// Storage
	DataDir string // env: DATA_DIR, default: "/data"
}

// Load creates a new Config with defaults and overrides from environment variables
func Load() *Config {
	c := &Config{
		// Server defaults
		BindAddr:    "0.0.0.0",
		Port:        8080,
		ExternalURL: "",

		// Engine selection defaults
		DefaultEngine: "torrserver",

		// Engine URL defaults
		TorrServerURL:    "http://torrserver:8090",
		RqbitURL:         "http://rqbit:3030",
		QBittorrentURL:   "http://qbittorrent:8080",
		QBitDownloadPath: "/downloads",
		QBitUsername:     "admin",
		QBitPassword:     "adminadmin",

		// Fetch proxy defaults
		DefaultFetchMethod: "sw_fallback",
		ProxyURL:           "",

		// Cache defaults
		CacheSizeGB:     60,
		CacheMaxAgeDays: 7,

		// Storage defaults
		DataDir: "/data",
	}

	// Override from environment variables
	if v := os.Getenv("BIND_ADDR"); v != "" {
		c.BindAddr = v
	}
	if v := os.Getenv("PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			c.Port = port
		}
	}
	if v := os.Getenv("BRIDGE_EXTERNAL_URL"); v != "" {
		c.ExternalURL = v
	}
	if v := os.Getenv("TORRENT_ENGINE"); v != "" {
		c.DefaultEngine = v
	}
	if v := os.Getenv("TORRSERVER_URL"); v != "" {
		c.TorrServerURL = v
	}
	if v := os.Getenv("TORRSERVER_USERNAME"); v != "" {
		c.TorrServerUsername = v
	}
	if v := os.Getenv("TORRSERVER_PASSWORD"); v != "" {
		c.TorrServerPassword = v
	}
	if v := os.Getenv("RQBIT_URL"); v != "" {
		c.RqbitURL = v
	}
	if v := os.Getenv("RQBIT_USERNAME"); v != "" {
		c.RqbitUsername = v
	}
	if v := os.Getenv("RQBIT_PASSWORD"); v != "" {
		c.RqbitPassword = v
	}
	if v := os.Getenv("QBITTORRENT_URL"); v != "" {
		c.QBittorrentURL = v
	}
	if v := os.Getenv("QBITTORRENT_DOWNLOAD_PATH"); v != "" {
		c.QBitDownloadPath = v
	}
	if v := os.Getenv("QBITTORRENT_USERNAME"); v != "" {
		c.QBitUsername = v
	}
	if v := os.Getenv("QBITTORRENT_PASSWORD"); v != "" {
		c.QBitPassword = v
	}
	if v := os.Getenv("DEFAULT_FETCH_METHOD"); v != "" {
		c.DefaultFetchMethod = v
	}
	if v := os.Getenv("PROXY_URL"); v != "" {
		c.ProxyURL = v
	}
	if v := os.Getenv("CACHE_SIZE_GB"); v != "" {
		if size, err := strconv.Atoi(v); err == nil {
			c.CacheSizeGB = size
		}
	}
	if v := os.Getenv("CACHE_MAX_AGE_DAYS"); v != "" {
		if age, err := strconv.Atoi(v); err == nil {
			c.CacheMaxAgeDays = age
		}
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		c.DataDir = v
	}

	return c
}

// LogSummary prints key configuration values for startup logging
func (c *Config) LogSummary() {
	fmt.Println("Configuration:")
	fmt.Printf("  Server:          %s:%d\n", c.BindAddr, c.Port)
	if c.ExternalURL != "" {
		fmt.Printf("  External URL:    %s\n", c.ExternalURL)
	} else {
		fmt.Printf("  External URL:    (will use Host header)\n")
	}
	fmt.Printf("  Default Engine:  %s\n", c.DefaultEngine)
	fmt.Println("  Engine URLs:")
	fmt.Printf("    TorrServer:    %s\n", c.TorrServerURL)
	fmt.Printf("    rqbit:         %s\n", c.RqbitURL)
	fmt.Printf("    qBittorrent:   %s\n", c.QBittorrentURL)
	fmt.Printf("  Fetch Method:    %s\n", c.DefaultFetchMethod)
	if c.ProxyURL != "" {
		fmt.Printf("  Proxy URL:       %s\n", c.ProxyURL)
	}
	fmt.Printf("  Cache:           %d GB, max age %d days\n", c.CacheSizeGB, c.CacheMaxAgeDays)
	fmt.Printf("  Data Directory:  %s\n", c.DataDir)
}
