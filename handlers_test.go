package main

// Handler tests that need the HTTP surface (SPEC §7).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setDataRoot swaps the global data root for the duration of the test (local
// sources resolve against it).
func setDataRoot(t *testing.T, dir string) {
	t.Helper()
	old := dataRoot
	dataRoot = dir
	t.Cleanup(func() { dataRoot = old })
}

// TestSequentialTwoRequests serves two consecutive GET /image requests against
// a sequential entry and asserts the ETag (content hash) changes between them —
// i.e. the endpoint advances one source per request.
func TestSequentialTwoRequests(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte("fake jpeg bytes "+f), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", f, err)
		}
	}

	resetImageCache(t)
	mu.Lock()
	old := seqIdx
	seqIdx = make(map[string]int64)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		seqIdx = old
		mu.Unlock()
	})
	setConfig(t, &Config{Images: []ImageEntry{
		{ID: "seq", Sources: []string{a, b}, Rotation: Rotation{Type: "sequential"}},
	}})

	get := func() string {
		rr := httptest.NewRecorder()
		handleImage(rr, httptest.NewRequest(http.MethodGet, "/image", nil))
		return rr.Header().Get("ETag")
	}
	first, second := get(), get()
	if first == "" || second == "" {
		t.Fatalf("missing ETag (first=%q second=%q)", first, second)
	}
	if first == second {
		t.Fatalf("ETag identical across two requests: %s", first)
	}
}

// TestHandleAPIStatusTimeline checks the /api/status JSON: the nested
// `timeline` (carousel slots, the next-change schedule, per-entry rows) and
// the theme shape.
func TestHandleAPIStatusTimeline(t *testing.T) {
	day := ImageEntry{ID: "day", Sources: []string{"/data/day.jpg"}}
	night := ImageEntry{
		ID:       "night",
		Sources:  []string{"/data/night.jpg"},
		Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"},
	}
	setConfig(t, &Config{Images: []ImageEntry{day, night}})

	rr := httptest.NewRecorder()
	handleAPIStatus(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Timeline Timeline `json:"timeline"`
		Theme    struct {
			Mode  string            `json:"mode"`
			Dark  map[string]string `json:"dark"`
			Light map[string]string `json:"light"`
		} `json:"theme"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v (body: %s)", err, rr.Body.String())
	}
	tl := resp.Timeline
	if tl.Timezone == "" {
		t.Error("timezone is empty")
	}
	if tl.Current.EntryID == "" || tl.Current.Condition == "" {
		t.Errorf("current = %+v, want a non-empty slot", tl.Current)
	}
	if tl.Prev.EntryID == "" || tl.Prev.Condition == "" {
		t.Errorf("prev = %+v, want a non-empty slot", tl.Prev)
	}
	if tl.Next.EntryID == "" || tl.Next.Condition == "" {
		t.Errorf("next = %+v, want a non-empty slot", tl.Next)
	}
	if tl.NextChange == nil || tl.NextChange.InSeconds <= 0 || tl.NextChange.At == "" {
		t.Errorf("next_change = %+v, want a scheduled change", tl.NextChange)
	}
	if len(tl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(tl.Entries))
	}
	if tl.Entries[0].ID != "day" || !tl.Entries[0].Active {
		t.Errorf("entries[0] = %+v, want active day", tl.Entries[0])
	}
	if tl.ActiveEntry == "" {
		t.Errorf("active_entry_id = %q, want the winner", tl.ActiveEntry)
	}

	// theme shape: mode + both palettes with defaults filled in
	if resp.Theme.Mode != "auto" {
		t.Errorf("theme.mode = %q, want auto", resp.Theme.Mode)
	}
	if resp.Theme.Dark["background"] != "#1a170f" || resp.Theme.Dark["foreground"] == "" || resp.Theme.Dark["accent"] == "" {
		t.Errorf("theme.dark = %+v, want defaults filled in", resp.Theme.Dark)
	}
	if resp.Theme.Light["background"] == "" || resp.Theme.Light["foreground"] == "" || resp.Theme.Light["accent"] == "" {
		t.Errorf("theme.light = %+v, want defaults filled in", resp.Theme.Light)
	}

	// wrong method → 405
	rr = httptest.NewRecorder()
	handleAPIStatus(rr, httptest.NewRequest(http.MethodPost, "/api/status", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}
}

// TestHandleAPIStatusTimelineSequential checks that a sequential winner's
// carousel slots (served inside /api/status) follow the in-memory cursor:
// current is the upcoming image (what /image serves right now), next the
// following step, prev the preceding one — and peeking does not advance the
// cursor.
func TestHandleAPIStatusTimelineSequential(t *testing.T) {
	mu.Lock()
	old := seqIdx
	seqIdx = make(map[string]int64)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		seqIdx = old
		mu.Unlock()
	})
	e := ImageEntry{
		ID:       "seq",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg", "/data/c.jpg"},
		Rotation: Rotation{Type: "sequential"},
	}
	setConfig(t, &Config{Images: []ImageEntry{e}})
	// simulate two dashboard GETs: cursor now at 2 → the last image /image
	// served is sources[1]; the next GET will serve sources[2] (the peek).
	mu.Lock()
	seqIdx["seq"] = 2
	mu.Unlock()

	rr := httptest.NewRecorder()
	handleAPIStatus(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Timeline Timeline `json:"timeline"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	tl := resp.Timeline
	if tl.Current.Source != "/data/b.jpg" {
		t.Errorf("current source = %q, want /data/b.jpg (last served)", tl.Current.Source)
	}
	if tl.Next.Source != "/data/c.jpg" {
		t.Errorf("next source = %q, want /data/c.jpg (upcoming)", tl.Next.Source)
	}
	if tl.Prev.Source != "/data/a.jpg" {
		t.Errorf("prev source = %q, want /data/a.jpg", tl.Prev.Source)
	}
	if tl.NextChange != nil {
		t.Errorf("next_change = %+v, want nil for sequential", tl.NextChange)
	}
	// the status peek must not have advanced the cursor
	if got := sequentialSource(e, false); got != "/data/c.jpg" {
		t.Errorf("cursor after status = %q, want peek /data/c.jpg unchanged", got)
	}
}

// TestHandleAPIConfigValidate checks the dry-run endpoint: valid YAML → 200,
// structurally broken YAML → 400 with the exact validation error, no write.
func TestHandleAPIConfigValidate(t *testing.T) {
	post := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/config/validate", strings.NewReader(body))
		handleAPIConfigValidate(rr, req)
		return rr
	}
	good := "images:\n  - id: a\n    sources: a.jpg\n"
	rr := post(good)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid config: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil || resp["valid"] != true {
		t.Errorf("valid config body = %s, want {\"valid\":true}", rr.Body.String())
	}

	bad := "images:\n  - id: a\n    sources: a.jpg\n    rotation:\n      type: interval\n"
	rr = post(bad)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid config: status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "interval") {
		t.Errorf("invalid config body = %q, want an interval-related error", rr.Body.String())
	}

	// unknown field must be rejected with its name
	rr = post("images:\n  - id: a\n    sources: a.jpg\n    rotation:\n      everydy: 30m\n")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown field") {
		t.Errorf("unknown field body = %q, want a strict-decoding error", rr.Body.String())
	}

	// wrong method → 405
	rr = httptest.NewRecorder()
	handleAPIConfigValidate(rr, httptest.NewRequest(http.MethodGet, "/api/config/validate", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rr.Code)
	}
}

// TestHandleAPIPreview checks /api/preview: serves bytes for a valid local
// source, ETag + If-None-Match round-trip to 304, 404 for a missing file,
// 400 for an invalid src, 405 for POST.
func TestHandleAPIPreview(t *testing.T) {
	dir := t.TempDir()
	setDataRoot(t, dir)
	resetPreviewCache(t)
	if err := os.WriteFile(filepath.Join(dir, "p.jpg"), []byte("\xff\xd8\xff fake jpeg"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	get := func(q string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handleAPIPreview(rr, httptest.NewRequest(http.MethodGet, "/api/preview"+q, nil))
		return rr
	}

	rr := get("?src=p.jpg")
	if rr.Code != http.StatusOK {
		t.Fatalf("existing file: status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "\xff\xd8\xff fake jpeg" {
		t.Errorf("body = %q, want the file bytes", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Error("ETag header missing")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}

	// revalidation: matching If-None-Match → 304 without a body
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/preview?src=p.jpg", nil)
	req.Header.Set("If-None-Match", etag)
	handleAPIPreview(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status = %d, want 304", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Errorf("If-None-Match: body = %q, want empty", rr.Body.String())
	}

	// stale etag → full 200 again
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/preview?src=p.jpg", nil)
	req.Header.Set("If-None-Match", "\"stale\"")
	handleAPIPreview(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("stale If-None-Match: status = %d, want 200", rr.Code)
	}

	rr = get("?src=missing.jpg")
	if rr.Code != http.StatusNotFound {
		t.Errorf("missing file: status = %d, want 404", rr.Code)
	}

	rr = get("?src=")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty src: status = %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	handleAPIPreview(rr, httptest.NewRequest(http.MethodPost, "/api/preview?src=p.jpg", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}
}

// TestHandleFavicon covers /favicon.svg: serves the app favicon as SVG,
// cached, with a 405 for other methods.
func TestHandleFavicon(t *testing.T) {
	rr := httptest.NewRecorder()
	handleFavicon(rr, httptest.NewRequest(http.MethodGet, "/favicon.svg", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q, want public, max-age=86400", cc)
	}
	if body := rr.Body.String(); body != faviconSVG {
		t.Errorf("body = %q, want the favicon SVG", body)
	}
	rr = httptest.NewRecorder()
	handleFavicon(rr, httptest.NewRequest(http.MethodPost, "/favicon.svg", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rr.Code)
	}
}

// TestPlaceholderServing covers the error placeholder (§6): a failing source
// with on_error "placeholder" serves the configured placeholder bytes; with
// no placeholder configured it serves the built-in transparent GIF; a
// configured placeholder that itself fails falls back to the built-in GIF.
func TestPlaceholderServing(t *testing.T) {
	dir := t.TempDir()
	setDataRoot(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "ph.jpg"), []byte("PH bytes"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	get := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handleImage(rr, httptest.NewRequest(http.MethodGet, "/image", nil))
		return rr
	}

	// configured placeholder → its bytes are served
	resetImageCache(t)
	setConfig(t, &Config{
		Placeholder: "ph.jpg",
		Images:      []ImageEntry{{ID: "bad", Sources: []string{"missing.jpg"}}},
	})
	rr := get()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != "PH bytes" {
		t.Errorf("body = %q, want the placeholder bytes", rr.Body.String())
	}

	// no placeholder → built-in transparent GIF
	resetImageCache(t)
	setConfig(t, &Config{Images: []ImageEntry{{ID: "bad", Sources: []string{"missing.jpg"}}}})
	rr = get()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), placeholderGIF) {
		t.Errorf("body = %q, want the built-in placeholderGIF", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/gif" {
		t.Errorf("Content-Type = %q, want image/gif", ct)
	}

	// configured placeholder that itself fails → built-in GIF fallback
	resetImageCache(t)
	setConfig(t, &Config{
		Placeholder: "also-missing.jpg",
		Images:      []ImageEntry{{ID: "bad", Sources: []string{"missing.jpg"}}},
	})
	rr = get()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !bytes.Equal(rr.Body.Bytes(), placeholderGIF) {
		t.Errorf("body = %q, want the built-in placeholderGIF fallback", rr.Body.String())
	}
}
