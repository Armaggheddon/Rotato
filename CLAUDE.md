# CLAUDE.md

Rotato — a tiny Go service that serves a rotating homepage image at `GET /image` for
homelab dashboards (Homepage, Dashy, Heimdall, ...). Selection is driven by
`config.yaml`: entries are evaluated top to bottom and the **last active entry wins**
(file order = priority). All times are UTC.

## Spec

The design is specified in [SPEC.md](SPEC.md) — the authoritative document.
Implement against it exactly; if code and spec diverge, update the spec.

## Hard constraints

- Go standard library + `gopkg.in/yaml.v3` only — no other dependencies.
- All application code in `package main`, split by concern: `main.go` (wiring),
  `config.go` (types/parsing/validation), `selection.go` (selection + timeline),
  `cache.go` (fetching, preview cache, placeholder), `handlers.go` (HTTP).
  `admin.html` embedded via `go:embed`.
- No logging, metrics, or auth. Exactly one startup line.
- Docker image built from `scratch`, ~6–8 MB.
- Internal clock is UTC arithmetic; wall-clock gates (`at`/`until`/`dates`)
  evaluate in the configured `tz` (default: `TZ` env or UTC). The tz database
  is embedded via `import _ "time/tzdata"`; interval cycles stay epoch-aligned.

## Commands

```bash
go build -o rotato .
CONFIG_PATH=./config.yaml ./rotato
docker build -t rotato .
docker run -p 8080:8080 -v $(pwd)/config:/app/config -v $(pwd)/data:/app/data rotato
docker compose up -d   # config mounted at /app/config, images at /app/data

# Target architecture: the build is parameterized by TARGETARCH (BuildKit's
# automatic platform arg, passed through by docker compose). Empty/unset =
# the build host's native arch, so everything above "just works" anywhere.
# For a Raspberry Pi (arm64) — even when building ON an x86 machine, since Go
# cross-compiles natively with CGO_ENABLED=0 (no QEMU/binfmt needed):
TARGETARCH=arm64 docker compose build
# move the image to the Pi (3 MB gzipped):
docker save rotato:latest | gzip > rotato-arm64.tar.gz
# ...copy the tarball over, then on the Pi:
gunzip -c rotato-arm64.tar.gz | docker load && docker compose up -d
```

## Conventions

- Durations accept Go strings (`"30m"`, `"6h"`) or integer seconds; times are `HH:MM`
  in the configured timezone; `dates` entries are `DD-MM` calendar days.
- Local `sources:` resolve against the data root (`DATA_ROOT`, default `/app/data`):
  `img.jpg` → `/app/data/img.jpg`; leading slashes are still data-relative.
- Rotation types: `static` | `interval` (cycle sources every `every`) | `daily`
  (active within `[at, until)`, wrapping past midnight when `until <= at`;
  optional `dates: ["DD-MM"]` limits it to those calendar days) |
  `sequential` (advance one source per GET, wrap around; in-memory cursor).
- `refresh` (default 1h) re-fetches remote sources; `on_error: skip | placeholder`.
  Serving is memory-bounded: local files are streamed from disk (never
  byte-cached; ETag from mtime+size), remote sources keep only the current +
  next image (`evictImageCache` after each serve, `prefetchNext` warms the
  next), errAt-only markers keep the 30s failure cooldown. The preview cache
  is separate and bounded at 64 entries.
  Global `placeholder:` (path or http(s) URL) is the error placeholder; it
  falls back to the built-in 1×1 transparent GIF when unset or unloadable.
- Selection logic lives in small pure functions (`entryActive`, `currentSource`,
  `selectEntry`) so tests can table-test it against fixed timestamps.
- Timeline planning (admin UI) is also pure and structural (`servedAt`,
  `conditionText`, `candidates`, `prevSlot`, `nextSlot`, `buildTimeline`) —
  no fetch checks; 48h horizon. Keep it that way when extending. It is served
  as the `timeline` field of `/api/status` (no separate endpoint).
- Admin UI (`admin.html`): self-contained, no CDN assets (no web fonts), no
  client YAML lib, favicon served from `/favicon.svg` (inline SVG, also the
  admin UI's icon). Single raw-YAML editor — the server is
  the validator. Terminal-TUI look; base colors come from config `theme:` —
  `mode: auto|dark|light` plus `dark:`/`light:` palettes (hex); the server
  fills defaults and serves the effective theme via `/api/status` (single 5s
  poll); derived shades use `color-mix()`. The editor gutter shows line
  numbers. The carousel previews go through a per-source blob cache: one
  `/api/preview` fetch per source, ETag/304 revalidation, 20s retry on
  failure, local entries dropped on config mtime change — unchanged sources
  are never re-fetched, so GIFs keep looping. Favicon and placeholder are
  not editable via the UI; theme (incl. mode) is YAML-only.
- Keep changes small and aligned with SPEC.md.
