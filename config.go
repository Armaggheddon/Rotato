package main

// Config: types, the built-in sample config, strict YAML parsing, structural
// validation (§4), timezone resolution, and the reload logic (§9).
//
// Parsing walks the yaml.Node tree manually so that unknown fields error and a
// scalar `sources:` coerces to a 1-element list.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// sampleConfig is the built-in starter config, auto-written to configPath on
// first start when no config file exists (§9). Kept as a Go constant so the
// project ships without a config.yaml file; paths match the container layout
// (/app/config + /app/data, see docker-compose.yml).
const sampleConfig = `# Rotato sample config — auto-created on first start at
# /app/config/config.yaml (the ./config host directory, mounted into the
# container). Evaluated top to bottom; the last active entry wins.
# Later entries override earlier ones.
# Local source paths are relative to the data root (/app/data): "day.jpg"
# means /app/data/day.jpg, "/path/img.jpg" means /app/data/path/img.jpg.

refresh: 1h
timeout: 10s
on_error: placeholder
# tz: Europe/Berlin   # IANA timezone for at/until/dates (default: TZ env or UTC)

# Error placeholder: served when the winning entry's source cannot be fetched
# and on_error is "placeholder". Local path or http(s) URL; falls back to a
# built-in 1x1 transparent GIF when it cannot be loaded itself.
# placeholder: placeholder.jpg

# Admin-UI theme. mode: auto | dark | light — "auto" follows the browser's
# dark/light preference. Omitted fields keep the built-in defaults below;
# the admin page picks the effective palette up from /api/status.
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
  # - id: christmas
  #   sources: christmas.jpg
  #   rotation:
  #     type: daily
  #     dates: ["25-12"]
  #     at: "00:00"
`

// Duration stores a length of time in seconds. 0 = unset/never.
type Duration int64

type Config struct {
	Refresh     *Duration    `yaml:"refresh"` // nil = default 1h; non-nil &0 = never
	Timeout     Duration     `yaml:"timeout"` // 0 = default 10s
	OnError     string       `yaml:"on_error"`
	Placeholder string       `yaml:"placeholder"` // error placeholder source (path or URL); "" = built-in GIF
	TZ          string       `yaml:"tz"`          // IANA timezone for at/until/dates; "" = TZ env or UTC
	Theme       *Theme       `yaml:"theme"`       // optional admin-UI theme; nil = defaults
	Images      []ImageEntry `yaml:"images"`
}

type ImageEntry struct {
	ID       string    `yaml:"id"`
	Sources  []string  `yaml:"sources"`
	Rotation Rotation  `yaml:"rotation"`
	Refresh  *Duration `yaml:"refresh"` // nil = inherit global default; non-nil 0 = never
	OnError  string    `yaml:"on_error"`
}

type Rotation struct {
	Type  string   `yaml:"type"`  // "static" | "interval" | "daily" | "sequential"; "" = static
	Every Duration `yaml:"every"` // 0 = unset
	At    string   `yaml:"at"`    // "HH:MM"
	Until string   `yaml:"until"` // "HH:MM"; "" = "00:00"
	Dates []string `yaml:"dates"` // "DD-MM" calendar dates gating a daily entry
}

// Theme controls the admin-UI look (§4.2): a display mode plus one palette
// per scheme. Mode "auto" follows the browser's dark/light preference.
type Theme struct {
	Mode  string  `yaml:"mode"`  // "auto" | "dark" | "light"; "" = auto
	Dark  Palette `yaml:"dark"`  // dark-scheme palette
	Light Palette `yaml:"light"` // light-scheme palette
}

// Palette is one scheme's three base colors. Empty fields fall back to the
// built-in defaults in defaultDarkPalette()/defaultLightPalette().
type Palette struct {
	Background string `yaml:"background"`
	Foreground string `yaml:"foreground"`
	Accent     string `yaml:"accent"`
}

// defaultDarkPalette is the built-in dark palette used when the dark block is
// omitted (or leaves fields empty). The shipped sample config spells out the
// same values.
func defaultDarkPalette() Palette {
	return Palette{Background: "#1a170f", Foreground: "#eceae5", Accent: "#2a9336"}
}

// defaultLightPalette is the built-in light palette (same role as above).
func defaultLightPalette() Palette {
	return Palette{Background: "#f5f3ee", Foreground: "#1f1d17", Accent: "#1f7a2e"}
}

// paletteFor resolves the effective palette for a display mode ("dark" or
// "light"), filling empty fields with the built-in defaults.
func (t Theme) paletteFor(mode string) Palette {
	d := defaultDarkPalette()
	p := t.Dark
	if mode == "light" {
		d = defaultLightPalette()
		p = t.Light
	}
	if p.Background == "" {
		p.Background = d.Background
	}
	if p.Foreground == "" {
		p.Foreground = d.Foreground
	}
	if p.Accent == "" {
		p.Accent = d.Accent
	}
	return p
}

// ---------------------------------------------------------------------------
// Loading & reloading (§9)
// ---------------------------------------------------------------------------

// loadConfig reads, parses, validates, and swaps in the config at path. On any
// error the last good config is kept and the error is recorded (§9).
func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			err = fmt.Errorf("%w — auto-seed likely failed: is the config directory writable by the container user?", err)
		}
		recordErr(err)
		return err
	}
	c, err := parseConfig(data)
	if err != nil {
		recordErr(err)
		return err
	}
	loc, err := resolveTZ(c.TZ)
	if err != nil {
		recordErr(err)
		return err
	}
	mu.Lock()
	config = c
	timeLoc = loc
	lastErr = nil
	// drop sequential cursors for entries that no longer exist
	for id := range seqIdx {
		gone := true
		for i := range c.Images {
			if c.Images[i].ID == id {
				gone = false
				break
			}
		}
		if gone {
			delete(seqIdx, id)
		}
	}
	mu.Unlock()
	refreshCache(c)
	// Pre-warm the winning source so the first request is fast; a failure here
	// is tolerated (it just populates the fetch-error cooldown).
	if e, src, err := selectEntry(time.Now().UTC(), ""); err == nil && e.ID != "" {
		getImageBytes(src)
	}
	return nil
}

func recordErr(err error) {
	mu.Lock()
	lastErr = err
	mu.Unlock()
}

// ---------------------------------------------------------------------------
// Strict parsing
// ---------------------------------------------------------------------------

func parseConfig(data []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("config is empty")
	}
	body, err := asMapping(root.Content[0])
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := rejectUnknown(body, "refresh", "timeout", "on_error", "placeholder", "tz", "theme", "images"); err != nil {
		return nil, err
	}
	c := &Config{}
	if v, ok := body["refresh"]; ok {
		d, err := parseDurationNode(v)
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		c.Refresh = &d
	}
	if v, ok := body["timeout"]; ok {
		d, err := parseDurationNode(v)
		if err != nil {
			return nil, fmt.Errorf("timeout: %w", err)
		}
		c.Timeout = d
	}
	if v, ok := body["on_error"]; ok {
		c.OnError = v.Value
	}
	if v, ok := body["placeholder"]; ok {
		c.Placeholder = v.Value
	}
	if v, ok := body["tz"]; ok {
		c.TZ = v.Value
	}
	if c.TZ != "" {
		if _, err := time.LoadLocation(c.TZ); err != nil {
			return nil, fmt.Errorf(tzErrFmt, c.TZ, err)
		}
	}
	if v, ok := body["theme"]; ok {
		t, err := parseTheme(v)
		if err != nil {
			return nil, err
		}
		c.Theme = t
	}
	if v, ok := body["images"]; ok {
		if v.Kind != yaml.SequenceNode {
			return nil, errors.New("images: must be a list")
		}
		for i, n := range v.Content {
			e, err := parseImageEntry(n)
			if err != nil {
				return nil, fmt.Errorf("images[%d]: %w", i, err)
			}
			c.Images = append(c.Images, *e)
		}
	}
	if err := validateConfig(c); err != nil {
		return nil, err
	}
	return c, nil
}

func parseImageEntry(n *yaml.Node) (*ImageEntry, error) {
	m, err := asMapping(n)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(m, "id", "sources", "rotation", "refresh", "on_error"); err != nil {
		return nil, err
	}
	e := &ImageEntry{}
	if v, ok := m["id"]; ok {
		e.ID = v.Value
	}
	if v, ok := m["sources"]; ok {
		sources, err := parseStringList(v, "sources")
		if err != nil {
			return nil, err
		}
		e.Sources = sources
	}
	if v, ok := m["rotation"]; ok {
		r, err := parseRotation(v)
		if err != nil {
			return nil, err
		}
		e.Rotation = *r
	}
	if v, ok := m["refresh"]; ok {
		d, err := parseDurationNode(v)
		if err != nil {
			return nil, fmt.Errorf("refresh: %w", err)
		}
		e.Refresh = &d
	}
	if v, ok := m["on_error"]; ok {
		e.OnError = v.Value
	}
	return e, nil
}

// parseTheme reads the optional admin-UI theme block (§4.2).
func parseTheme(n *yaml.Node) (*Theme, error) {
	m, err := asMapping(n)
	if err != nil {
		return nil, fmt.Errorf("theme: %w", err)
	}
	if err := rejectUnknown(m, "mode", "dark", "light"); err != nil {
		return nil, fmt.Errorf("theme: %w", err)
	}
	t := &Theme{}
	if v, ok := m["mode"]; ok {
		t.Mode = v.Value
	}
	if v, ok := m["dark"]; ok {
		p, err := parsePalette(v)
		if err != nil {
			return nil, fmt.Errorf("theme.dark: %w", err)
		}
		t.Dark = *p
	}
	if v, ok := m["light"]; ok {
		p, err := parsePalette(v)
		if err != nil {
			return nil, fmt.Errorf("theme.light: %w", err)
		}
		t.Light = *p
	}
	return t, nil
}

// parsePalette reads a {background, foreground, accent} color block.
func parsePalette(n *yaml.Node) (*Palette, error) {
	m, err := asMapping(n)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknown(m, "background", "foreground", "accent"); err != nil {
		return nil, err
	}
	p := &Palette{}
	if v, ok := m["background"]; ok {
		p.Background = v.Value
	}
	if v, ok := m["foreground"]; ok {
		p.Foreground = v.Value
	}
	if v, ok := m["accent"]; ok {
		p.Accent = v.Value
	}
	return p, nil
}

func parseRotation(n *yaml.Node) (*Rotation, error) {
	m, err := asMapping(n)
	if err != nil {
		return nil, fmt.Errorf("rotation: %w", err)
	}
	if err := rejectUnknown(m, "type", "every", "at", "until", "dates"); err != nil {
		return nil, err
	}
	r := &Rotation{}
	if v, ok := m["type"]; ok {
		r.Type = v.Value
	}
	if v, ok := m["every"]; ok {
		d, err := parseDurationNode(v)
		if err != nil {
			return nil, fmt.Errorf("every: %w", err)
		}
		r.Every = d
	}
	if v, ok := m["at"]; ok {
		r.At = v.Value
	}
	if v, ok := m["until"]; ok {
		r.Until = v.Value
	}
	if v, ok := m["dates"]; ok {
		ds, err := parseStringList(v, "dates")
		if err != nil {
			return nil, err
		}
		r.Dates = ds
	}
	return r, nil
}

func asMapping(n *yaml.Node) (map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping, got a %s", nodeKind(n.Kind))
	}
	m := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("expected a scalar key, got a %s", nodeKind(k.Kind))
		}
		if _, dup := m[k.Value]; dup {
			return nil, fmt.Errorf("duplicate field %q", k.Value)
		}
		m[k.Value] = v
	}
	return m, nil
}

func rejectUnknown(m map[string]*yaml.Node, allowed ...string) error {
	for k := range m {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("unknown field %q", k)
		}
	}
	return nil
}

func nodeKind(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.DocumentNode:
		return "document"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}

// parseStringList accepts a scalar (1-element list) or a sequence of strings;
// what names the field in error messages ("sources", "dates").
func parseStringList(n *yaml.Node, what string) ([]string, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("%s: entries must be strings", what)
			}
			out = append(out, item.Value)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: must be a string or a list of strings", what)
}

// parseDurationNode accepts a Go duration string ("30m", "6h"), a plain
// integer (seconds), or the literal "never" (= 0) for refresh (§4.5).
func parseDurationNode(n *yaml.Node) (Duration, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, errors.New("expected a scalar duration")
	}
	s := n.Value
	if s == "never" {
		return 0, nil
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return Duration(secs), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use a Go duration like \"30m\" or integer seconds)", s)
	}
	return Duration(d / time.Second), nil
}

// ---------------------------------------------------------------------------
// Structural validation (§4.6)
// ---------------------------------------------------------------------------

// hexColor matches a "#RRGGBB" hex color (theme values).
var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// validateConfig rejects structurally broken configs (§4.6).
func validateConfig(c *Config) error {
	if c.Placeholder != "" && !validSource(c.Placeholder) {
		return fmt.Errorf("placeholder %q must be a non-empty path or an http(s):// URL", c.Placeholder)
	}
	if c.Theme != nil {
		switch c.Theme.Mode {
		case "", "auto", "dark", "light":
		default:
			return fmt.Errorf("theme.mode %q must be \"auto\", \"dark\", or \"light\"", c.Theme.Mode)
		}
		for scheme, p := range map[string]Palette{
			"dark":  c.Theme.Dark,
			"light": c.Theme.Light,
		} {
			for field, v := range map[string]string{
				"background": p.Background,
				"foreground": p.Foreground,
				"accent":     p.Accent,
			} {
				if v != "" && !hexColor.MatchString(v) {
					return fmt.Errorf("theme.%s.%s %q must be a hex color like \"#1a170f\"", scheme, field, v)
				}
			}
		}
	}
	seen := make(map[string]bool, len(c.Images))
	for i := range c.Images {
		e := &c.Images[i]
		if e.ID == "" {
			return fmt.Errorf("images[%d]: id is required", i)
		}
		if seen[e.ID] {
			return fmt.Errorf("duplicate id %q", e.ID)
		}
		seen[e.ID] = true
		if len(e.Sources) == 0 {
			return fmt.Errorf("entry %q: sources must not be empty", e.ID)
		}
		for _, s := range e.Sources {
			if !validSource(s) {
				return fmt.Errorf("entry %q: source %q must be a non-empty path or an http(s):// URL", e.ID, s)
			}
		}
		r := &e.Rotation
		if r.At != "" {
			if _, ok := parseHM(r.At); !ok {
				return fmt.Errorf("entry %q: `at` must be \"HH:MM\" (in tz), got %q", e.ID, r.At)
			}
		}
		if r.Until != "" {
			if _, ok := parseHM(r.Until); !ok {
				return fmt.Errorf("entry %q: `until` must be \"HH:MM\" (in tz), got %q", e.ID, r.Until)
			}
		}
		if r.Every < 0 {
			return fmt.Errorf("entry %q: `every` must be positive", e.ID)
		}
		switch r.Type {
		case "", "static":
			if len(e.Sources) > 1 {
				return fmt.Errorf("entry %q: static with %d sources — use `interval`", e.ID, len(e.Sources))
			}
			if r.Every > 0 {
				return fmt.Errorf("entry %q: `every` with a single source — add more sources or drop `every`", e.ID)
			}
			if len(r.Dates) > 0 {
				return fmt.Errorf("entry %q: `dates` requires `type: daily`", e.ID)
			}
		case "interval":
			if len(e.Sources) < 2 {
				return fmt.Errorf("entry %q: interval needs at least 2 sources — use `static`, or `refresh` if you want to re-download one changing image", e.ID)
			}
			if r.Every <= 0 {
				return fmt.Errorf("entry %q: interval requires `every` > 0", e.ID)
			}
			if len(r.Dates) > 0 {
				return fmt.Errorf("entry %q: `dates` requires `type: daily`", e.ID)
			}
		case "daily":
			if r.At == "" {
				return fmt.Errorf("entry %q: daily requires `at`", e.ID)
			}
			if len(e.Sources) >= 2 && r.Every <= 0 {
				return fmt.Errorf("entry %q: daily with %d sources needs `every` — add `every` to cycle within the window, or use a single source", e.ID, len(e.Sources))
			}
			if len(e.Sources) == 1 && r.Every > 0 {
				return fmt.Errorf("entry %q: `every` with a single source — add more sources or drop `every`", e.ID)
			}
			for _, d := range r.Dates {
				if err := checkDate(d); err != nil {
					return fmt.Errorf("entry %q: dates: %w", e.ID, err)
				}
			}
		case "sequential":
			if len(e.Sources) < 2 {
				return fmt.Errorf("entry %q: sequential needs at least 2 sources — use `static` for a single source", e.ID)
			}
			if r.Every > 0 {
				return fmt.Errorf("entry %q: sequential advances on every request — `every` is not applicable", e.ID)
			}
			if e.OnError == "skip" {
				return fmt.Errorf("entry %q: on_error `skip` is not supported with sequential — use `placeholder`", e.ID)
			}
			if len(r.Dates) > 0 {
				return fmt.Errorf("entry %q: `dates` requires `type: daily`", e.ID)
			}
		default:
			return fmt.Errorf("entry %q: unknown rotation type %q", e.ID, r.Type)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Time & date helpers (§4.4, §4.5)
// ---------------------------------------------------------------------------

// parseHM parses "HH:MM" into minutes since midnight; ok=false for anything
// that is not exactly "HH:MM" (the format gate for `at`/`until`).
func parseHM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// checkDate validates a `dates` element: exactly "DD-MM" and a real calendar
// date (rejects silent normalization like "31-02"). "29-02" is accepted — it
// simply never matches in non-leap years.
func checkDate(s string) error {
	if len(s) != 5 || s[2] != '-' {
		return fmt.Errorf("date %q must be exactly \"DD-MM\"", s)
	}
	d, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || m < 1 || m > 12 || d < 1 || d > 31 {
		return fmt.Errorf("date %q must be exactly \"DD-MM\"", s)
	}
	t := time.Date(2000, time.Month(m), d, 0, 0, 0, 0, time.UTC) // 2000 is a leap year
	if t.Day() != d || int(t.Month()) != m {
		return fmt.Errorf("date %q is not a real calendar date", s)
	}
	return nil
}

// tzErrFmt is the shared invalid-timezone error (parseConfig validates the
// configured tz; resolveTZ re-checks it plus the TZ env fallback).
const tzErrFmt = "tz %q is not a valid IANA timezone (e.g. \"Europe/Berlin\", \"America/New_York\"): %w"

// resolveTZ resolves the timezone used for the `at`/`until`/`dates` wall-clock
// gates: the config `tz:` wins, then the TZ environment variable (the
// timezone the operator/host provides), then UTC.
func resolveTZ(name string) (*time.Location, error) {
	if name == "" {
		name = os.Getenv("TZ")
	}
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf(tzErrFmt, name, err)
	}
	return loc, nil
}
