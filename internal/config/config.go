package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// Config holds all configuration for the stremio-torrent-bridge
type Config struct {
	mu sync.Mutex

	// Server
	BindAddr    string // env: BIND_ADDR, default: "0.0.0.0"
	Port        int    // env: PORT, default: 8080
	ExternalURL string // env: BRIDGE_EXTERNAL_URL, default: "" (will fallback to Host header)

	// Engine selection. User-settable from the UI; persisted in /data/config.json.
	// Not driven by env vars — first-boot default is set in Load() and any
	// subsequent change via PUT /api/config becomes the new persisted value.
	DefaultEngine string

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
	DefaultFetchMethod string // env: DEFAULT_FETCH_METHOD, default: "direct"
	ProxyURL           string // env: PROXY_URL, default: "" (for custom proxy fetch method)

	// Cache
	CacheSizeGB     int // env: CACHE_SIZE_GB, default: 60
	CacheMaxAgeDays int // env: CACHE_MAX_AGE_DAYS, default: 7

	// Storage
	DataDir string // env: DATA_DIR, default: "/data"

	persistentLoaded bool
}

// persistentFields is the on-disk JSON shape: only user-settable fields.
type persistentFields struct {
	DefaultEngine      *string `json:"defaultEngine,omitempty"`
	DefaultFetchMethod *string `json:"defaultFetchMethod,omitempty"`
	ProxyURL           *string `json:"proxyURL,omitempty"`
	CacheSizeGB        *int    `json:"cacheSizeGB,omitempty"`
	CacheMaxAgeDays    *int    `json:"cacheMaxAgeDays,omitempty"`
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
		DefaultFetchMethod: "tab_relay",
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

	// Overlay persisted values last so they win over env defaults.
	if err := c.LoadPersistent(); err != nil {
		fmt.Printf("config: load persistent: %v\n", err)
	}

	return c
}

// LoadPersistent reads the persisted JSON file (if present) and overlays its
// values onto the config. Missing file is not an error.
func (c *Config) LoadPersistent() error {
	data, err := os.ReadFile(c.persistentPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var p persistentFields
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("parse persistent config: %w", err)
	}

	if p.DefaultEngine != nil {
		c.DefaultEngine = *p.DefaultEngine
	}
	if p.DefaultFetchMethod != nil {
		c.DefaultFetchMethod = *p.DefaultFetchMethod
	}
	if p.ProxyURL != nil {
		c.ProxyURL = *p.ProxyURL
	}
	if p.CacheSizeGB != nil {
		c.CacheSizeGB = *p.CacheSizeGB
	}
	if p.CacheMaxAgeDays != nil {
		c.CacheMaxAgeDays = *p.CacheMaxAgeDays
	}
	c.persistentLoaded = true
	return nil
}

// Save writes user-settable config fields to a JSON file under DataDir.
// Write is atomic (temp file + rename) and mutex-guarded for concurrent callers.
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	engine := c.DefaultEngine
	fetch := c.DefaultFetchMethod
	proxy := c.ProxyURL
	size := c.CacheSizeGB
	age := c.CacheMaxAgeDays

	p := persistentFields{
		DefaultEngine:      &engine,
		DefaultFetchMethod: &fetch,
		ProxyURL:           &proxy,
		CacheSizeGB:        &size,
		CacheMaxAgeDays:    &age,
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal persistent config: %w", err)
	}

	path := c.persistentPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write persistent config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename persistent config: %w", err)
	}
	return nil
}

func (c *Config) persistentPath() string {
	return c.DataDir + "/config.json"
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
	if c.persistentLoaded {
		fmt.Printf("  Persistent:      loaded from %s\n", c.persistentPath())
	} else {
		fmt.Printf("  Persistent:      none (first boot or no overrides)\n")
	}
}
