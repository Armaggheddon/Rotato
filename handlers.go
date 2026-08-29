package main

// HTTP API (§7): /image, /admin, /favicon.svg, /api/config, /api/config/validate, /api/preview, /api/status, /health.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	entry, src, err := selectEntry(now, r.URL.Query().Get("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// sequential rotation advances once per GET; HEAD serves the peek without
	// consuming a step, and id-preview shows the upcoming source (no advance):
	// first hit serves sources[0], the next sources[1], ...
	if entry.Rotation.Type == "sequential" {
		if r.URL.Query().Get("id") != "" {
			src = sequentialSource(entry, false)
		} else if r.Method == http.MethodGet {
			src = sequentialSource(entry, true)
		}
	}
	// Local files are streamed straight from disk (never byte-cached); remote
	// sources are served from the bounded cache.
	if isRemote(src) {
		data, ct, err := getRemoteBytes(src)
		if err != nil {
			servePlaceholder(w, r, entry)
			return
		}
		w.Header().Set("X-Image-ID", entry.ID)
		w.Header().Set("Cache-Control", "no-cache")
		if !writeETagged(w, r, data, ct) && r.Method != http.MethodHead {
			w.Write(data)
		}
	} else if err := serveLocalSource(w, r, src, entry); err != nil {
		servePlaceholder(w, r, entry)
	}
	// Bound the serving cache to current + next and warm the next remote
	// source so the follow-up request is a hit.
	evictImageCache(entry, src, now)
	prefetchNext(entry, src, now)
}

// servePlaceholder writes the on_error outcome for a failing source:
// 404 when the entry's policy is "skip", the placeholder bytes otherwise.
func servePlaceholder(w http.ResponseWriter, r *http.Request, entry ImageEntry) {
	if entry.OnError == "skip" {
		http.NotFound(w, r)
		return
	}
	data, ct := placeholderBytes()
	w.Header().Set("X-Image-ID", entry.ID)
	w.Header().Set("Cache-Control", "no-cache")
	if !writeETagged(w, r, data, ct) && r.Method != http.MethodHead {
		w.Write(data)
	}
}

// serveLocalSource streams a local file from disk without caching its bytes:
// the ETag derives from mtime+size plus the source identity, so revalidation
// stays cheap, 304s still work, and distinct sources never share a tag. It
// returns an error only when the file cannot be opened or stat'd; response
// errors after that are unrecoverable (headers are sent).
func serveLocalSource(w http.ResponseWriter, r *http.Request, src string, entry ImageEntry) error {
	f, err := os.Open(resolveSourcePath(src))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h := fnv.New32a()
	h.Write([]byte(src))
	etag := fmt.Sprintf("\"%x-%x-%x\"", st.ModTime().UnixNano(), st.Size(), h.Sum32())
	w.Header().Set("Content-Type", contentTypeForFile(f, src))
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Image-ID", entry.ID)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
	return nil
}

// writeETagged sets the entity headers (Content-Type, ETag, nosniff) and
// writes a 304 when the client's If-None-Match matches the current bytes.
// It reports whether the response was already written.
func writeETagged(w http.ResponseWriter, r *http.Request, data []byte, ct string) bool {
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	return false
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(adminHTML)
}

// faviconSVG is the app favicon — the same mark the admin UI uses, now served
// once from /favicon.svg so dashboards (Homepage's favicon: setting, browser
// tabs) can reference it.
const faviconSVG = `<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'><rect width='16' height='16' fill='#1a170f'/><text x='8' y='12' font-family='monospace' font-size='11' font-weight='bold' fill='#2a9336' text-anchor='middle'>R</text></svg>`

// handleFavicon serves the app favicon as SVG. Static content, so it can be
// cached aggressively.
func handleFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Content-Length", strconv.Itoa(len(faviconSVG)))
	w.Write([]byte(faviconSVG))
}

func handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		data, err := os.ReadFile(configPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)
	case http.MethodPost:
		data, err := readConfigBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := parseConfig(data); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		tmp := configPath + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.Rename(tmp, configPath); err != nil {
			http.Error(w, "rename failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := loadConfig(configPath); err != nil {
			http.Error(w, "saved, but reload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("config saved\n"))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	mu.RLock()
	c := config
	loc := timeLoc
	cacheCount := len(imageCache)
	var lastErrVal any
	if lastErr != nil {
		lastErrVal = lastErr.Error()
	}
	mu.RUnlock()
	tl := Timeline{}
	if c != nil {
		tl = buildTimeline(c, now, loc)
		// A sequential winner's real prev/current/next live in the in-memory
		// cursor, not the structural timeline: patch the slots with a peek so
		// the carousel shows what /image actually serves around the cursor.
		if tl.ActiveEntry != "" {
			for i := range c.Images {
				e := &c.Images[i]
				if e.ID == tl.ActiveEntry && e.Rotation.Type == "sequential" {
					prev, cur, next := sequentialSlots(*e, conditionText(*e))
					tl.Prev, tl.Current, tl.Next = prev, cur, next
					tl.NextChange = nil // advances per request, not per time
					break
				}
			}
		}
	}
	mtime := ""
	if st, err := os.Stat(configPath); err == nil {
		mtime = st.ModTime().UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"utc_now":         now.Format(time.RFC3339),
		"timezone":        loc.String(),
		"active_entry_id": tl.ActiveEntry,
		"active_source":   tl.ActiveSource,
		"config_path":     configPath,
		"config_mtime":    mtime,
		"last_error":      lastErrVal,
		"cache_entries":   cacheCount,
		"theme":           themeForStatus(),
		"timeline":        tl,
	})
}

// themeForStatus resolves the effective admin-UI theme (display mode plus
// both dark/light palettes with defaults filled in) for /api/status.
func themeForStatus() map[string]any {
	mu.RLock()
	c := config
	mu.RUnlock()
	var t Theme
	if c != nil && c.Theme != nil {
		t = *c.Theme
	}
	mode := t.Mode
	if mode == "" {
		mode = "auto"
	}
	pal := func(p Palette) map[string]string {
		return map[string]string{
			"background": p.Background,
			"foreground": p.Foreground,
			"accent":     p.Accent,
		}
	}
	return map[string]any{
		"mode":  mode,
		"dark":  pal(t.paletteFor("dark")),
		"light": pal(t.paletteFor("light")),
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("OK"))
}

// readConfigBody reads the config body, capped at 1 MiB (shared by the save
// and validate endpoints).
func readConfigBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("request body: %w", err)
	}
	return data, nil
}

// handleAPIConfigValidate is a dry-run of handleAPIConfig's POST: it parses
// and validates the body but never writes or reloads anything. 200 on a valid
// config, 400 with the exact validation error otherwise.
func handleAPIConfigValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data, err := readConfigBody(w, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := parseConfig(data); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"valid": true})
}

// handleAPIPreview serves the bytes of one source (local path or remote URL)
// from the dedicated preview cache, so the admin UI can preview sources that
// are not part of the saved config yet. ETag + If-None-Match keep polling
// transfers down to 304s.
func handleAPIPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	src := r.URL.Query().Get("src")
	if !validSource(src) {
		http.Error(w, "invalid src: must be a non-empty path or an http(s):// URL", http.StatusBadRequest)
		return
	}
	data, ct, err := previewCached(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// no-cache: the ETag keeps the admin UI's polls down to 304 responses.
	w.Header().Set("Cache-Control", "no-cache")
	if writeETagged(w, r, data, ct) {
		return
	}
	w.Write(data)
}
