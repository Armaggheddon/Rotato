# Rotato — Design Specification

## 1. Overview

Rotato serves a single rotating image at `GET /image` for homelab dashboard widgets
(Homepage, Dashy, Heimdall, ...). The user provides images — local files or remote
URLs — in a YAML config; the service decides which image is currently shown based on
per-entry rotation policies.

Design principles:

- **File order is the only priority.** The config is evaluated top to bottom; the
  last entry that is active at the current moment wins. Later entries override
  earlier ones — there is no `priority` field and no tie-breaking.
- **Wall-clock times follow the configured timezone.** The container clock is
  UTC, but `at`/`until`/`dates` are interpreted in the location set by `tz:`
  (default: the `TZ` environment variable, else UTC). Interval cycles stay
  epoch-aligned Unix seconds and are timezone-independent.
- **Three orthogonal concerns per entry:** which source is shown (rotation), when
  the entry is active (time gate), and how fresh the bytes are (`refresh`).
  Keeping them separate is what makes the config hand-writable.

## 2. Constraints

- Go standard library **plus** `gopkg.in/yaml.v3` only (the sole external dependency).
- All application code in `package main`, split by concern across a handful of
  files (see §3); `admin.html` embedded via `go:embed`; the sample config is a
  Go string constant (`sampleConfig`) — no `config.yaml` file is shipped with
  the project.
- Final Docker image built from `scratch`, target size ~6–8 MB.
- No logging framework, no metrics, no authentication. Only a single startup line.
- Timezone support is opt-in via `tz:` (IANA name); the tz database is embedded
  in the binary (`time/tzdata`), so it works in the scratch image with no extra
  files. Without `tz:`, wall-clock gates evaluate in the `TZ` env location, or
  UTC when that is unset too.

## 3. Directory structure

```
project-root/
├── main.go             (wiring: embed, globals, watcher, entry point)
├── config.go           (config types, strict YAML parsing, validation, reload)
├── selection.go        (image selection + timeline planning, pure functions)
├── cache.go            (caching & fetching)
├── handlers.go         (HTTP API)
├── main_test.go        (shared test helpers)
├── config_test.go      (parsing/validation tests)
├── selection_test.go   (selection + timeline tests)
├── handlers_test.go    (HTTP handler tests)
├── admin.html          (embedded via go:embed)
├── README.md           (user-facing guide: run, build, config reference)
├── docs/               (README assets: logo, UI screenshots)
├── SPEC.md             (this document)
├── CLAUDE.md           (project instructions for the agent)
├── go.mod
├── go.sum
├── Dockerfile
└── docker-compose.yml
```

## 4. Configuration (`config.yaml`)

### 4.1 Evaluation model

- The `images` list is evaluated **top to bottom on every request**.
- Each entry is either *active* at the current moment or not.
- The **last active entry wins** and its current source is served.
- If a winning entry's source cannot be fetched, `on_error` decides (see §6).
- If no entry is active at all: `404 Not Found`.

### 4.2 Global options

```yaml
refresh: 1h            # default re-fetch cadence for remote sources.
                       #   `never` (or 0) = fetch once, cache forever.
                       #   Default when omitted: 1h.
timeout: 10s           # per-fetch HTTP timeout. Default: 10s.
on_error: placeholder  # global default for entries that don't override:
                       #   placeholder | skip. Default: placeholder.
placeholder: ph.jpg    # optional error placeholder (local path or http(s)
                       #   URL) served when the winning entry's source cannot
                       #   be fetched and on_error is "placeholder". Falls
                       #   back to a built-in 1x1 transparent GIF when
                       #   unset or unloadable. Default: unset.
tz: Europe/Berlin      # IANA timezone for at/until/dates. Default: the TZ
                       #   environment variable if set, else UTC.
theme:                 # optional admin-UI theme. `mode` picks the active
                       #   palette: auto | dark | light ("auto" follows the
                       #   browser's dark/light preference). Omitted colors
                       #   keep the built-in defaults below; the admin page
                       #   applies the effective values served by /api/status.
  mode: auto
  dark:
    background: "#1a170f"
    foreground: "#eceae5"
    accent: "#2a9336"
  light:
    background: "#f5f3ee"
    foreground: "#1f1d17"
    accent: "#1f7a2e"
```

### 4.3 Entry fields

```yaml
images:
  - id: evening              # required, unique (used by ?id= and the admin UI)
    sources:                 # required. A single path/URL may be written as a
      - /data/sunsets/a.jpg  #   scalar; a scalar coerces to a 1-element list.
      - /data/sunsets/b.jpg

    rotation:                # optional; omitted = static
      type: static           # static | interval | daily | sequential
      every: 30m             # interval: how long each source is shown (required)
                             # daily:    optional — cycle sources while the
                             #           window is active (needs >= 2 sources)
      at: "18:00"            # daily: window start, HH:MM (required)
      until: "06:00"         # daily: window end, optional (default: midnight)
      dates: ["25-12"]       # daily: optional calendar dates (DD-MM) the entry
                             #   is limited to; combines with the time window

    refresh: 15m             # optional per-entry override of global `refresh`
                             #   (remote sources only)
    on_error: skip           # optional per-entry override of global `on_error`
```

| Option | Applies to | Semantics |
|---|---|---|
| `sources` | all | Local path or remote `http(s)://` URL, scalar or YAML list. Local paths are resolved against the **data root** (`DATA_ROOT`, default `/app/data`): `img.jpg` → `/app/data/img.jpg`, `/path/img.jpg` → `/app/data/path/img.jpg`. A leading slash does not make the path container-absolute. |
| `rotation.type: static` | 1 source | Always active, never changes. The base-layer entry. Also the implicit default when `rotation` is omitted. |
| `rotation.type: interval` + `every` | ≥2 sources | Always active; cycles the list: `index = (unix / every) % len(sources)`. Epoch-aligned → deterministic across restarts and replicas, no state to store. |
| `rotation.type: sequential` | ≥2 sources | Always active; advances one step **per GET request**: first hit serves `sources[0]`, next `sources[1]`, ... wrapping around. In-memory state (per-entry cursor), reset on restart; HEAD requests and `?id=` previews do not advance. |
| `rotation.type: daily` + `at` | 1 source | Active from `at` until midnight (in `tz`). The override mechanism — place it *after* the always-active entries it should shadow. |
| `rotation.type: daily` + `at` + `until` | 1 source | Active within `[at, until)`. If `until <= at`, the window wraps past midnight (e.g. `at: "22:00", until: "06:00"` = evening + night). |
| `rotation.type: daily` + `dates` | ≥1 source | Date gate: active only on the listed `DD-MM` calendar days (evaluated in `tz`). Combines with the time window — both must match. `at: "00:00"` with the default `until` = all day. |
| `rotation.type: daily` + `every` | ≥2 sources | Composition: while the daily window is active, also cycle the sources every `every`. |
| `refresh` | remote | Re-download cadence for *changing* content (camera snapshots, weather maps). Distinct from `every`: `every` picks **which** source, `refresh` updates the **bytes** of one source. |
| `on_error` | all | Fetch-failure behavior for this entry: `skip` (treat the entry as inactive this request, so the next earlier active entry is served) or `placeholder` (serve the built-in 1×1 transparent GIF). |

### 4.4 Daily window rule & date gate

The daily gate is evaluated against the **wall clock in the configured
timezone** (`tz`, default `TZ` env or UTC), so `at`/`until`/`dates` mean what
they say on a local clock.

Time window, in minutes since local midnight: active iff `at <= mins < until`.
When `until <= at` the window wraps past midnight: active iff
`mins >= at || mins < until`. (`at == until` = the full 24 hours.) DST edge
days follow wall-clock semantics: an hour skipped by a spring-forward
transition never matches; an hour repeated by a fall-back transition matches
twice.

Date gate (`dates`, optional): active only when the local calendar date is in
the list (`DD-MM`, recurring every year). `29-02` is accepted but never matches
in non-leap years. When both a time window and a date gate are present, both
must match.

### 4.5 Formats

- Times: `"HH:MM"`, 24-hour, interpreted in the configured `tz`
  (`time.Parse("15:04", ...)`).
- Dates: `"DD-MM"` (day-month), recurring every year; e.g. `"25-12"`.
- Durations (`every`, `refresh`, `timeout`): Go duration strings (`"30m"`, `"6h"`,
  `"3600s"`, `"24h"`) or a plain integer, which means seconds. `never` is accepted
  for `refresh` (= cache forever).

### 4.6 Validation — structural only

File order resolves conflicts by construction, so validation only rejects configs
that are *structurally* broken. All of these produce a clear error message
(surfaced in the `POST /api/config` 400 body and recorded for the admin UI):

- Strict decoding: unknown fields are errors (a typo like `everyday:` must not
  silently become `static`).
- Duplicate `id` — reject (ambiguous `?id=`).
- Empty `sources` — reject.
- A source that is neither a path (`/`, `./`, `../`) nor an `http(s)://` URL — reject.
- `every` unparsable or `<= 0` — reject.
- `at` / `until` not `"HH:MM"` — reject.
- A `dates` element that is not a real `DD-MM` calendar date — reject
  ("31-02" and friends are rejected up front; "29-02" is accepted and simply
  never matches in non-leap years).
- A `theme` color (in `dark:` or `light:`) that is not a `#RRGGBB` hex
  color — reject with hint: "must be a hex color like \"#1a170f\"" (empty
  fields are fine: they keep the built-in defaults). A `theme.mode` other
  than `auto`/`dark`/`light` — reject. Unknown keys inside `theme:` or its
  palette blocks are rejected like any other unknown field (the old flat
  `theme: {background, foreground, accent}` shape is no longer accepted).
- A `placeholder` that is not a non-empty path or `http(s)://` URL — reject.
- `dates` on a rotation type other than `daily` — reject with hint:
  "`dates` requires `type: daily`".
- `interval` with a single source — reject with hint: "use `static`, or `refresh`
  if you want to re-download one changing image".
- `daily` with ≥2 sources and no `every` — reject with hint: "add `every` to
  cycle within the window, or use a single source".
- `every` combined with a single source (any type) — reject.
- `static` with ≥2 sources — reject with hint: "use `interval`".
- `sequential` with a single source — reject with hint: "use `static`".
- `sequential` with `every` — reject (it advances per request, not per time).
- `sequential` with `on_error: skip` — reject (skip pre-checks the current
  source, which is unknowable before the advance; use `placeholder`).

### 4.7 Sample config

The built-in sample config lives as the Go string constant `sampleConfig` in
`config.go` (written to `CONFIG_PATH` on first start, §9). Local source paths are
data-root relative (§4.1): `day.jpg` means `/app/data/day.jpg`.

```yaml
# /app/config/config.yaml — evaluated top to bottom; the last active entry wins.
# Wall-clock times (at/until/dates) follow tz (default: TZ env or UTC).
# Local paths are relative to /app/data: "day.jpg" = /app/data/day.jpg.

refresh: 1h
timeout: 10s
on_error: placeholder
# tz: Europe/Berlin

# Error placeholder: served when the winning source cannot be fetched and
# on_error is "placeholder". Local path or http(s) URL; falls back to a
# built-in 1x1 transparent GIF when it cannot be loaded itself.
# placeholder: placeholder.jpg

# Admin-UI theme. mode: auto | dark | light — "auto" follows the browser's
# dark/light preference. Omitted colors keep the built-in defaults below.
theme:
  mode: auto
  dark:
    background: "#1a170f"
    foreground: "#eceae5"
    accent: "#2a9336"
  light:
    background: "#f5f3ee"
    foreground: "#1f1d17"
    accent: "#1f7a2e"

images:
  # base layer — always active unless overridden below
  - id: day
    sources: day.jpg

  # remote camera snapshot: re-fetched hourly; if it's down, "day" shows instead
  - id: backyard-cam
    sources: http://camera.lan/snapshot.jpg
    refresh: 1h
    on_error: skip

  # from 18:00 to midnight (in tz) this overrides the entries above,
  # cycling two sunsets every 30 min while active
  - id: evening
    sources:
      - sunsets/a.jpg
      - sunsets/b.jpg
    rotation:
      type: daily
      at: "18:00"
      every: 30m

  # wraps midnight: active 22:00–24:00 and 00:00–06:00, overrides everything above
  - id: night
    sources: night.jpg
    rotation:
      type: daily
      at: "22:00"
      until: "06:00"

  # Christmas background: all day on 25-12 (overrides everything above that day)
  - id: christmas
    sources: christmas.jpg
    rotation:
      type: daily
      dates: ["25-12"]
      at: "00:00"
```

## 5. Image selection logic

Per request:

1. `now = time.Now().UTC()`.
2. Walk `images` top to bottom. An entry is active when:
   - `static` → always.
   - `interval` → always (it always has a current source; the cycle picks which).
   - `sequential` → always (the per-entry cursor picks which source).
   - `daily` → `now` falls in its window (rule §4.4).
   The **last active entry** is the winner. An entry whose fetch fails with
   `on_error: skip` counts as inactive for this request (so the walk naturally
   lands on the previous active entry).
3. The winner's current source:
   - with `every`: `sources[(unix / every) % len(sources)]`
   - `sequential`: advance the per-entry cursor one step (only on GET without
     `?id=`; see §5.4) and serve `sources[cursor % len(sources)]`
   - without: `sources[0]`
4. Obtain bytes via the cache/fetch logic (§6) and serve.
5. No active entry → `404 Not Found`.

`?id=<id>` bypass: find the entry by id (unknown id → 404). Serve that entry's
current source, **ignoring its time gate** — so the admin UI preview and a pinned
Homepage widget always show something. The cycle still advances, so
`?id=evening` returns the correct 30-minute slot. In id mode, `on_error: skip`
degrades to `placeholder` (there is no meaningful "previous entry"), and a
`sequential` entry serves the upcoming source (its cursor position) without
advancing, so previewing never skips an image.

## 6. Caching & fetching

Memory-bounded serving: **local files are never byte-cached** (streamed from
disk per request), and **remote sources keep at most the current + next
source's bytes** at any time. All entries live in `map[source]cacheEntry`
where `cacheEntry = {data, contentType, fetchedAt, errAt}`, guarded by a
`sync.RWMutex`; `errAt`-only markers (a failed fetch) are cheap and kept for
the cooldown.

- **Local files**: streamed from disk on every request (`os.Open` + `io.Copy`,
  `Content-Length` from stat). The ETag derives from `mtime+size`, so
  `If-None-Match` revalidation stays cheap and returns 304s without hashing
  large files. Nothing is held in memory; edits on disk are picked up
  immediately.
- **Remote URLs**: fetched on first access with `timeout` (default 10s).
  Re-fetched when `now - fetchedAt >= refresh` (per-entry or global default, 1h).
  `refresh: never` = fetch once, cache forever.
- **Current + next bound**: after each `/image` request the serving cache is
  evicted down to the current source and the next one in rotation order
  (`evictImageCache`); the next remote source is then pre-fetched
  best-effort in the background (`prefetchNext`), so the follow-up request is
  a cache hit. Memory therefore stays at ≤ 2 remote images regardless of the
  config's size, plus the Go runtime baseline.
- **Stale-on-failure**: if a refresh fetch fails but the cache still holds bytes,
  serve the stale bytes (better than a placeholder for cameras). Failure handling
  (`on_error`) only applies when the cache has no bytes for the source.
- **Failure cooldown**: a failed fetch is remembered for 30s (`errAt`); requests
  within the cooldown do not re-attempt — this prevents a 10s-timeout pile-up on a
  dead source. The `on_error: skip` pre-check probes remote sources through the
  cache and local sources with a cheap `stat` (never a full read).
- **Placeholder**: the configured `placeholder:` source is fetched through
  the normal cache (global `refresh` cadence) when remote, or read directly
  when local; on failure — or when no placeholder is configured — the
  hard-coded 1×1 transparent GIF is served as `image/gif`.
- **Preview cache** (`/api/preview`): a separate bounded map (max 64 entries,
  oldest evicted first) so previews never pollute the serving cache. Remote
  previews follow the same refresh/stale-on-failure/cooldown rules as the
  serving cache; local previews are dropped on each config reload so they
  re-read. This is what keeps the admin UI's carousel GIFs looping across
  polls.
- **Eviction on reload**: a successful config reload drops serving-cache
  entries whose source is no longer referenced (local sources hold no bytes,
  so nothing else needs invalidating) and drops local preview entries.
- **Content-Type**: for local files, map by extension (`.png .jpg .jpeg .gif
  .webp`); otherwise sniff `http.DetectContentType` (first 512 bytes when
  streaming). For remote responses, use the response `Content-Type` header
  when it is `image/*`, else sniff. Supported formats: PNG, JPEG, GIF, WebP.

## 7. HTTP API

| Endpoint | Method | Description |
|---|---|---|
| `/image` | GET | Currently active image (raw bytes) with correct `Content-Type`. Query params: `?id=<id>` (bypass gate, serve that entry's current source). |
| `/image` | HEAD | Headers only (same headers as GET). |
| `/admin` | GET | Serves the admin UI HTML. |
| `/favicon.svg` | GET | The app favicon as SVG (same mark the admin UI uses) so dashboards such as Homepage can reference it as an app icon. Static, served with `Cache-Control: public, max-age=86400`. |
| `/api/config` | GET | Returns the current `config.yaml` content as plain text. |
| `/api/config` | POST | Validates the request body (§4.6), writes it atomically to `CONFIG_PATH` (`<path>.tmp` + rename), triggers a reload, returns `200 OK` — or `400` with the error details. Body capped at 1 MB (`http.MaxBytesReader`). |
| `/api/config/validate` | POST | Dry-run of the POST above: parses and validates the body but never writes or reloads. `200` with `{"valid":true}` on a valid config, `400` with the exact validation error otherwise. Powers the admin UI's live validity indicator. Body capped at 1 MB. |
| `/api/preview?src=` | GET | Raw bytes of one source (local path or remote URL) from the dedicated preview cache (`previewCached`), so the admin UI can preview sources that are not part of the saved config yet. `ETag` (SHA-256 of the bytes) + `If-None-Match` → `304` keep polling transfers minimal. `400` for an invalid `src`; `404` when the fetch fails. |
| `/api/status` | GET | JSON: `{utc_now, timezone, active_entry_id, active_source, config_path, config_mtime, last_error, cache_entries, theme, timeline}` — the debugging aid for "why is my dashboard showing the wrong picture" plus everything the admin UI polls. `theme` carries the effective theme (`mode` plus `dark`/`light` palettes, defaults filled in); `timeline` is the full timeline payload: per-entry conditions, the prev/current/next carousel slots, and the next scheduled change (`next_change.at` RFC3339 UTC + `in_seconds`). See §8. (Formerly served by the removed `/api/timeline`.) |
| `/health` | GET | `200 OK` (plain text). |

`/image` response headers: `Content-Type`, `ETag` (hash of the bytes),
`X-Image-ID` (winning entry id), `Cache-Control: no-cache` (rotation may change
any second; the ETag keeps transfers to 304s). Honor `If-None-Match` → `304`.

## 8. Admin UI (`admin.html`)

Single self-contained HTML file, embedded via `go:embed`, no external CDN
assets (no web fonts, no client-side YAML library; the favicon is an inline
SVG data URI). Lightweight **terminal-like TUI** look: monospace with
contextual ligatures, square corners, accent-bordered buttons, a blinking
title cursor, and colors driven by the config `theme:` block (§4.2). The
effective theme (mode + both palettes, defaults filled in) is served via
`/api/status` and applied as CSS custom properties; `mode: auto` follows the
OS `prefers-color-scheme` with a live listener. All shades in between
(panels, borders, dim text) derive from the three base colors with
`color-mix()`, so the whole UI follows the user's palette.

Layout, top to bottom:

- **Header**: title and a timezone/UTC clock.
- **Carousel**: three slanted (`skewX`) slots — previous, current, next —
  **slightly overlapped** (the side slots tuck under the current frame via
  negative margins + z-index). The side slots are clipped previews with a
  slight blur; the current slot is larger, carries an accent border and a ✓
  badge. All three slots render their **exact sources** through the preview
  blob cache; for a sequential winner the server patches the slots in
  `/api/status` to follow the in-memory cursor (current = the image `/image`
  last served, next/prev = the adjacent sources). Empty slots show a dashed
  "no active entry" state; failed previews get a ✕ badge.
- **Timeline** aligned to the three carousel columns: hairline vertical
  markers (accent dot + border for the winner) with the activation condition
  of the prev/current/next image, the served source, and a live "next change
  in Xm Ys" countdown driven by `next_change.at`. A chip row lists every
  entry with its condition, highlighting the current winner.
- **Editor**: the raw `config.yaml` in a textarea with a **gutter** showing
  line numbers. The gutter scrolls in sync with the textarea; line numbers
  update on every keystroke.
- **Footer**: config path, mtime, cache-entry count from `/api/status`; the
  banner shows `last_error` when set.

The client preview loader is a **blob cache keyed by source**: one
`GET /api/preview` per source, one shared objectURL reused by the three
carousel slots. Unchanged sources are never re-fetched, so GIFs keep looping
across polls; revalidation uses `If-None-Match` (304 keeps the existing
blob). Failed previews show a ✕ badge and retry every 20 s while still
visible. A config `mtime` change drops the local-file preview entries (their
bytes may have changed on disk).
The server remains the only validator: editing marks the buffer dirty and
triggers a debounced `POST /api/config/validate` whose result shows as a
validity indicator next to the Save button; **Save** (Ctrl/Cmd+S also works)
POSTs the raw text to `/api/config` — hand-written comments are preserved
because the editor is the raw file.

Polling: a single `/api/status` poll every 5 s drives the carousel, header,
footer, banner, and theme; `/api/config` is re-fetched only when the editor
is not dirty. A successful save reloads everything immediately.

## 9. Auto-reload

- Background goroutine: `time.NewTicker(5 * time.Second)`; compare `os.Stat`
  `ModTime()` + `Size()` with the last seen values.
- **Debounce**: only reload when the stat values are unchanged across two
  consecutive ticks (guards against editors that write non-atomically — vim swap
  files, half-written YAML).
- `loadConfig(path)`:
  1. Read + strict-parse + validate (§4.6).
  2. On any error: **keep the last good config**, record the error (exposed via
     `/api/status` and the admin UI). Never blank the running config.
  3. On success: swap the config (under mutex), evict stale cache entries
     (§6), pre-warm the winning source (fetch now, tolerate failure).
- Startup: if `CONFIG_PATH` does not exist, write the built-in sample config
  (the `sampleConfig` Go constant, §4.7) to that path and continue
  (fresh-volume friendly); the watcher picks up later edits.
  If the seed write fails (e.g. the mounted directory is not writable by the
  container user), the error is recorded and exposed via `/api/status` and the
  admin UI.
- `POST /api/config` calls `loadConfig()` after the atomic write (the watcher will
  also see the change and reload once more — harmless).

## 10. Dockerfile (scratch, ~6–8 MB)

```dockerfile
# Stage 1: Builder
FROM golang:1.27-alpine AS builder
# Target CPU architecture. BuildKit sets TARGETARCH automatically to the build
# host's arch; docker compose passes it through as a build arg (see
# docker-compose.yml), so `TARGETARCH=arm64 docker compose build` cross-builds
# an arm64 image (Raspberry Pi) even on an x86 machine — Go cross-compiles
# natively with CGO_ENABLED=0, no QEMU/binfmt needed.
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -a -installsuffix cgo -ldflags="-s -w" -o rotato .

# Stage 2: Scratch
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/rotato /rotato
ENTRYPOINT ["/rotato"]
```

Notes:

- **Architecture**: the build is parameterized by `TARGETARCH` — BuildKit's
  automatic platform arg (build host's arch unless overridden), passed through
  as a build arg by `docker compose`. Unset/empty means "build natively for
  this machine"; `TARGETARCH=arm64 docker compose build` cross-builds for a
  Raspberry Pi on any host (Go cross-compiles with `CGO_ENABLED=0`, no QEMU).
  Transfer the image with `docker save ... | gzip` + `docker load` on the
  target, or push/pull via a registry.

- The tz database is embedded in the binary (`time/tzdata`), so `tz:` works in
  the scratch image with no extra files. If the config has no `tz:`, the `TZ`
  environment variable (if set) supplies the location.
- `ca-certificates.crt` is still required for HTTPS fetches.
- The config path defaults to `/app/config/config.yaml` (overridable via the
  `CONFIG_PATH` env var for local runs, §12) and the port `8080` is fixed in
  `main.go`. `DATA_ROOT` (default `/app/data`) sets where local sources
  resolve (§4.1) and may be overridden via env if images are mounted elsewhere.
- **Mount the config's directory, not the file**: `-v $(pwd)/config:/app/config`
  (plus `-v $(pwd)/data:/app/data` for the image files). A single-file bind
  mount breaks the atomic-rename write (§7) after the first POST (the container
  keeps the dead inode). The directory mount keeps admin-UI saves persistent
  across container recreation.

## 11. Implementation notes

- `package main`; `//go:embed admin.html`; structs `Config`, `ImageEntry`,
  `Rotation`, `cacheEntry`; globals `config *Config`, `timeLoc`, `lastErr`,
  the caches `imageCache`/`previewCache`, `seqIdx` cursors, and `lastStat`,
  all guarded by `mu sync.RWMutex`.
- File layout (§3): `main.go` (embed, globals, `main`, `watcher`),
  `config.go` (types, `sampleConfig`, `loadConfig`, strict parsing,
  `validateConfig`, `resolveTZ`), `selection.go` (`entryActive`,
  `currentSource`, `sequentialSource`, `selectEntry`, plus the timeline
  planners `servedAt`, `conditionText`, `fmtDur`, `candidates`, `prevSlot`,
  `nextSlot`, `buildTimeline`), `cache.go` (`getImageBytes`,
  `localBytes`/`remoteBytes` — shared by the serving and preview caches —,
  `placeholderBytes`, `previewCached`, `refreshCache`, `resolveSourcePath`),
  `handlers.go` (all HTTP handlers).
- Custom YAML unmarshaling for `sources` (scalar-or-list) and for durations
  (duration-string-or-integer-seconds).
- `theme:` parsing is strict like everything else (`parseTheme` +
  `parsePalette`, unknown keys rejected, `mode` and `#RRGGBB` hex validated);
  `Theme.paletteFor(mode)` fills the defaults and `themeForStatus()`
  (handlers.go) exposes the effective mode + both palettes on `/api/status`.
- Timeline planning is **structural** (no fetch checks) so it stays pure and
  table-testable; a failing `on_error: "skip"` entry can therefore diverge
  from what `/image` actually serves. The carousel renders the real image
  through the preview cache regardless.
- `previewCached` backs `/api/preview` from the dedicated bounded preview
  cache; `candidates` scans daily window starts/ends (respecting the dates
  gate) and cycle boundaries over a 48 h horizon (forward and backward).
- `main()`: config path from `CONFIG_PATH` env (local runs) with the
  container default `/app/config/config.yaml`, port fixed at `8080`,
  `DATA_ROOT` (default `/app/data`, §4.1) read from env (baked for the
  container, §10), seed the sample config if missing, load config, start
  watcher, start HTTP server with a `ReadHeaderTimeout`, print
  `Server started on :8080` — the only startup line.
- `time.Now().UTC()` remains the internal clock; only the `at`/`until`/`dates`
  wall-clock gates are interpreted through the resolved location (§4.4).

## 12. Testing & running

```bash
# Build & run locally
go build -o rotato .
CONFIG_PATH=./config.yaml ./rotato

# Exercise it
curl -s localhost:8080/image -o img.jpg
curl -s localhost:8080/api/status
curl -i -X POST --data-binary @config.yaml localhost:8080/api/config

# Docker
docker build -t rotato .
docker run -p 8080:8080 -v $(pwd)/config:/app/config -v $(pwd)/data:/app/data rotato

# Docker Compose (config mounted at /app/config, images at /app/data)
docker compose up -d

# Cross-build for a Raspberry Pi (arm64) on any machine, then move the image:
TARGETARCH=arm64 docker compose build
# ...copy the tarball to the Pi, then load it there and start:
docker save rotato:latest | gzip > rotato-arm64.tar.gz   # on the build machine
gunzip -c rotato-arm64.tar.gz | docker load              # on the Pi
docker compose up -d
```

The compose file runs the container as `PUID:PGID` (default `1000:1000`,
Homepage-style, set via `.env` or the environment), so every file the service
creates — the auto-seeded config and admin-UI saves — is owned by that host
user. Create `./config` and `./data` owned by that user before first start.

Admin UI: `http://localhost:8080/admin`.

Unit tests (`main_test.go`): table-driven tests for `entryActive`,
`currentSource`, and `selectEntry` against fixed timestamps — especially the
daily-window wrap rule and epoch-aligned interval indexing. No external test
deps.

## 13. Explicitly out of scope

- **Authentication** — LAN trust model. (Optional `ADMIN_TOKEN` header check on
  `POST /api/config` is a noted future extension.)
- **Logging, metrics** — by constraint; `/api/status` is the observability surface.
- **Image resizing/transcoding** — would break the stdlib-only constraint; serve
  raw bytes and document PNG/JPEG/GIF/WebP support (§6).
- **Categories / multi-tenant filtering** — `?id=` covers per-widget pinning;
  revisit only if multi-tenant use appears.
- **Priority numbers, timezones, multi-file configs** — replaced by design
  (§1) or deleted.
