package main

// Rotato — a tiny Go service that serves a rotating homepage image at
// GET /image (SPEC.md is the authoritative design document).
//
// File layout (all package main):
//
//	main.go      — wiring: embed, globals, watcher, entry point
//	config.go    — config types, strict YAML parsing, validation, reload
//	selection.go — image selection logic (pure functions)
//	cache.go     — caching & fetching
//	handlers.go  — HTTP API

import (
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
	_ "time/tzdata" // embed the tz database so tz: works in the scratch image
)

//go:embed admin.html
var adminHTML []byte

// statInfo is the watcher's last-seen file stat (modtime + size).
type statInfo struct {
	modTime time.Time
	size    int64
}

var (
	config       *Config
	mu           sync.RWMutex // guards config, timeLoc, lastErr, seqIdx, imageCache, previewCache
	imageCache   map[string]cacheEntry
	previewCache map[string]cacheEntry // /api/preview bytes (separate from the serving cache)
	lastStat     statInfo
	lastErr      error
	timeLoc      *time.Location = time.UTC // location for at/until/dates wall-clock gates

	configPath string           // CONFIG_PATH
	dataRoot   string           // base dir for local sources (DATA_ROOT, default /app/data)
	seqIdx     map[string]int64 // sequential-rotation cursor per entry id (guarded by mu)
)

// ---------------------------------------------------------------------------
// Auto-reload watcher (§9)
// ---------------------------------------------------------------------------

// watcher polls the config file every 5s and reloads only when the stat
// (ModTime + Size) is unchanged across two consecutive ticks (debounce).
func watcher() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var pending bool
	for range ticker.C {
		st, err := os.Stat(configPath)
		if err != nil {
			pending = false
			continue
		}
		cur := statInfo{modTime: st.ModTime(), size: st.Size()}
		if cur != lastStat {
			lastStat = cur
			pending = true
			continue
		}
		if pending {
			loadConfig(configPath)
			pending = false
		}
	}
}

// ---------------------------------------------------------------------------
// Entry point (§11)
// ---------------------------------------------------------------------------

func main() {
	// CONFIG_PATH is for local runs; the container default is /app/config.
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		configPath = v
	} else {
		configPath = "/app/config/config.yaml"
	}
	port := "8080"
	if v := os.Getenv("DATA_ROOT"); v != "" {
		dataRoot = v
	} else {
		dataRoot = "/app/data"
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(sampleConfig), 0o644); err != nil {
			recordErr(fmt.Errorf("cannot auto-create config %s: %w — check that the mounted directory is writable by the container user (PUID/PGID)", configPath, err))
		}
	}
	imageCache = make(map[string]cacheEntry)
	previewCache = make(map[string]cacheEntry)
	seqIdx = make(map[string]int64)
	loadConfig(configPath)
	if st, err := os.Stat(configPath); err == nil {
		lastStat = statInfo{modTime: st.ModTime(), size: st.Size()}
	}
	go watcher()
	mux := http.NewServeMux()
	mux.HandleFunc("/image", handleImage)
	mux.HandleFunc("/admin", handleAdmin)
	mux.HandleFunc("/favicon.svg", handleFavicon)
	mux.HandleFunc("/api/config", handleAPIConfig)
	mux.HandleFunc("/api/config/validate", handleAPIConfigValidate)
	mux.HandleFunc("/api/preview", handleAPIPreview)
	mux.HandleFunc("/api/status", handleAPIStatus)
	mux.HandleFunc("/health", handleHealth)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("Server started on :%s\n", port)
	if err := srv.ListenAndServe(); err != nil {
		os.Exit(1)
	}
}
