package main

// Shared test helpers. hm builds fixed UTC instants; setConfig swaps the
// global config for the duration of a test; resetImageCache and
// resetPreviewCache replace the shared caches so a hit from an earlier
// subtest can never make a missing file appear servable; mustLoc loads an
// IANA location.

import (
	"testing"
	"time"
)

// hm returns the UTC instant "HH:MM" on 1970-01-01.
func hm(h, m int) time.Time {
	return time.Unix(int64(h*3600+m*60), 0).UTC()
}

// setConfig swaps the global config for the duration of the test and restores
// the previous one afterwards.
func setConfig(t *testing.T, c *Config) {
	t.Helper()
	mu.Lock()
	old := config
	config = c
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		config = old
		mu.Unlock()
	})
}

// resetImageCache replaces the shared fetch cache so a hit from an earlier
// subtest can never make a missing file appear servable.
func resetImageCache(t *testing.T) {
	t.Helper()
	mu.Lock()
	imageCache = make(map[string]cacheEntry)
	mu.Unlock()
}

// resetPreviewCache replaces the shared preview cache (same role as
// resetImageCache for /api/preview).
func resetPreviewCache(t *testing.T) {
	t.Helper()
	mu.Lock()
	previewCache = make(map[string]cacheEntry)
	mu.Unlock()
}

// mustLoc loads an IANA location or fails the test (tzdata is embedded in the
// binary, so real zones load everywhere).
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}
