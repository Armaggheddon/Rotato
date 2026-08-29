package main

// Caching & fetching (§6): in-memory byte cache keyed by source, with
// refresh cadence, stale-on-failure, and a 30s failure cooldown.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// placeholderGIF is the built-in 1x1 transparent GIF served when an entry's
// source cannot be fetched and its on_error policy is "placeholder".
var placeholderGIF = []byte{
	0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0x01, 0x00, 0x01, 0x00, 0x80, 0x00,
	0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0x21, 0xf9, 0x04, 0x01, 0x00,
	0x00, 0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x02,
	0x02, 0x44, 0x01, 0x00, 0x3b,
}

// cacheEntry is one source's cached bytes plus fetch bookkeeping.
type cacheEntry struct {
	data        []byte
	contentType string
	fetchedAt   time.Time
	errAt       time.Time
}

func getImageBytes(src string) ([]byte, string, error) {
	if isRemote(src) {
		return remoteBytes(src, imageCache, imageCachePut)
	}
	return localBytes(src, imageCache, imageCachePut)
}

// placeholderBytes resolves the error placeholder (§6): the configured
// `placeholder:` source served through the normal image cache, or the
// built-in 1x1 transparent GIF when no placeholder is configured or the
// configured one cannot be fetched.
func placeholderBytes() ([]byte, string) {
	mu.RLock()
	p := ""
	if config != nil {
		p = config.Placeholder
	}
	mu.RUnlock()
	if p != "" {
		if data, ct, err := getImageBytes(p); err == nil {
			return data, ct
		}
	}
	return placeholderGIF, "image/gif"
}

// previewCacheMax bounds the preview cache: preview sources are unbounded
// user input (anything the admin UI ever typed), so the map is capped and
// the oldest entries are evicted on overflow.
const previewCacheMax = 64

// previewCached returns bytes for the admin UI's /api/preview endpoint. It
// uses a dedicated cache (never the serving imageCache): sources may not be
// part of the saved config, and previews must not pollute what /image
// serves. Remote sources follow the configured refresh cadence with
// stale-on-failure; local files are read on first access and cached until
// the next config reload.
func previewCached(src string) ([]byte, string, error) {
	if isRemote(src) {
		return remoteBytes(src, previewCache, previewCachePut)
	}
	return localBytes(src, previewCache, previewCachePut)
}

// localBytes serves a local file through the given cache: read on first
// access, cached until the next config reload (refreshCache drops local
// entries so they re-read), with a 30s failure cooldown.
func localBytes(src string, cache map[string]cacheEntry, put func(string, cacheEntry)) ([]byte, string, error) {
	now := time.Now().UTC()
	mu.RLock()
	c, ok := cache[src]
	mu.RUnlock()
	if ok {
		if len(c.data) > 0 {
			return c.data, c.contentType, nil
		}
		if now.Sub(c.errAt) < 30*time.Second {
			return nil, "", errors.New("recent read failure for " + src)
		}
	}
	data, err := os.ReadFile(resolveSourcePath(src))
	if err != nil {
		put(src, cacheEntry{errAt: now})
		return nil, "", err
	}
	ct := contentTypeForPath(src, data)
	put(src, cacheEntry{data: data, contentType: ct, fetchedAt: now})
	return data, ct, nil
}

// remoteBytes serves a remote URL through the given cache: re-fetches when
// the refresh cadence has elapsed, serves stale bytes when a refresh fails,
// and remembers failures for a 30s cooldown (§6).
func remoteBytes(src string, cache map[string]cacheEntry, put func(string, cacheEntry)) ([]byte, string, error) {
	now := time.Now().UTC()
	mu.RLock()
	c, ok := cache[src]
	mu.RUnlock()
	if ok && len(c.data) > 0 {
		if now.Sub(c.errAt) < 30*time.Second {
			return c.data, c.contentType, nil // failure cooldown: serve stale
		}
		refresh := refreshFor(src)
		if refresh < 0 || now.Sub(c.fetchedAt) < refresh {
			return c.data, c.contentType, nil
		}
		data, ct, err := fetch(src)
		if err != nil {
			// Stale-on-failure: keep serving the cached bytes.
			put(src, cacheEntry{data: c.data, contentType: c.contentType, fetchedAt: c.fetchedAt, errAt: now})
			return c.data, c.contentType, nil
		}
		put(src, cacheEntry{data: data, contentType: ct, fetchedAt: now})
		return data, ct, nil
	}
	if ok && now.Sub(c.errAt) < 30*time.Second {
		return nil, "", errors.New("recent fetch failure for " + src)
	}
	data, ct, err := fetch(src)
	if err != nil {
		put(src, cacheEntry{errAt: now})
		return nil, "", err
	}
	put(src, cacheEntry{data: data, contentType: ct, fetchedAt: now})
	return data, ct, nil
}

// previewCachePut stores one preview entry, evicting the oldest entry when
// the cache is full.
func previewCachePut(src string, e cacheEntry) {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := previewCache[src]; !ok && len(previewCache) >= previewCacheMax {
		oldestSrc := ""
		var oldest time.Time
		for s, c := range previewCache {
			if oldest.IsZero() || c.fetchedAt.Before(oldest) {
				oldestSrc, oldest = s, c.fetchedAt
			}
		}
		if oldestSrc != "" {
			delete(previewCache, oldestSrc)
		}
	}
	previewCache[src] = e
}

// imageCachePut stores one entry in the serving cache.
func imageCachePut(src string, e cacheEntry) {
	mu.Lock()
	imageCache[src] = e
	mu.Unlock()
}

// refreshFor returns the refresh cadence for src: per-entry override, else the
// global default, else 1h. A negative value means "never re-fetch".
func refreshFor(src string) time.Duration {
	mu.RLock()
	c := config
	mu.RUnlock()
	if c == nil {
		return time.Hour
	}
	g := time.Hour
	if c.Refresh != nil {
		if *c.Refresh <= 0 {
			g = -1
		} else {
			g = time.Duration(*c.Refresh) * time.Second
		}
	}
	for i := range c.Images {
		for _, s := range c.Images[i].Sources {
			if s == src {
				if c.Images[i].Refresh != nil {
					if *c.Images[i].Refresh <= 0 {
						return -1
					}
					return time.Duration(*c.Images[i].Refresh) * time.Second
				}
				return g
			}
		}
	}
	return g
}

func httpTimeout() time.Duration {
	mu.RLock()
	c := config
	mu.RUnlock()
	if c != nil && c.Timeout > 0 {
		return time.Duration(c.Timeout) * time.Second
	}
	return 10 * time.Second
}

func fetch(src string) ([]byte, string, error) {
	client := &http.Client{Timeout: httpTimeout()}
	resp, err := client.Get(src)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch %s: unexpected status %s", src, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 64<<20 {
		return nil, "", fmt.Errorf("fetch %s: body exceeds 64 MiB", src)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "image/") {
		ct = http.DetectContentType(data)
	}
	return data, ct, nil
}

// refreshCache runs after a successful config reload: drops entries whose
// source is no longer referenced and invalidates local-file entries so they
// are re-read on the next access (§6, §9). Preview entries get the same
// treatment: local files re-read, remote sources keep their bytes.
func refreshCache(c *Config) {
	refd := make(map[string]bool)
	for i := range c.Images {
		for _, s := range c.Images[i].Sources {
			refd[s] = true
		}
	}
	if c.Placeholder != "" {
		refd[c.Placeholder] = true
	}
	mu.Lock()
	for src := range imageCache {
		if !refd[src] || !isRemote(src) {
			delete(imageCache, src)
		}
	}
	for src := range previewCache {
		if !isRemote(src) {
			delete(previewCache, src)
		}
	}
	mu.Unlock()
}

func isRemote(src string) bool {
	return strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://")
}

func validSource(s string) bool {
	if isRemote(s) {
		return true
	}
	// any non-empty local path is valid: it resolves against the data root,
	// with or without a leading slash (§4.1)
	return strings.TrimSpace(s) != ""
}

// resolveSourcePath resolves a local source against the data root (DATA_ROOT,
// default /app/data). Every local path — with or without a leading slash — is
// interpreted as relative to dataRoot: "img.jpg" and "/img.jpg" both mean
// /app/data/img.jpg, "/path/img.jpg" means /app/data/path/img.jpg. Remote
// URLs pass through unchanged.
func resolveSourcePath(src string) string {
	if isRemote(src) {
		return src
	}
	return filepath.Join(dataRoot, src)
}

func contentTypeForPath(p string, data []byte) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return http.DetectContentType(data)
}
