package main

// Tests for config parsing/validation helpers (SPEC §4.4, §4.5): resolveTZ
// and checkDate. Full parseConfig coverage is exercised indirectly through
// the /api/config handler tests; the selection-logic tables live in
// selection_test.go.

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestResolveTZ checks the timezone resolution order: config tz > TZ env > UTC,
// and that invalid names are rejected.
func TestResolveTZ(t *testing.T) {
	// config value wins over the environment
	t.Setenv("TZ", "Asia/Tokyo")
	loc, err := resolveTZ("Europe/Berlin")
	if err != nil {
		t.Fatalf("resolveTZ(Europe/Berlin): %v", err)
	}
	if loc.String() != "Europe/Berlin" {
		t.Fatalf("resolveTZ(Europe/Berlin) = %q", loc.String())
	}
	// empty config falls back to the TZ env var
	loc, err = resolveTZ("")
	if err != nil {
		t.Fatalf("resolveTZ(\"\") with TZ set: %v", err)
	}
	if loc.String() != "Asia/Tokyo" {
		t.Fatalf("resolveTZ(\"\") with TZ=Asia/Tokyo = %q", loc.String())
	}
	// no config, no env → UTC
	t.Setenv("TZ", "")
	loc, err = resolveTZ("")
	if err != nil || loc != time.UTC {
		t.Fatalf("resolveTZ(\"\") with no TZ = %v, %v", loc, err)
	}
	// invalid name is an error
	if _, err := resolveTZ("Not/AZone"); err == nil {
		t.Fatal("resolveTZ(Not/AZone) = nil error, want error")
	}
}

// TestCheckDate covers the DD-MM format and calendar-date validation.
func TestCheckDate(t *testing.T) {
	valid := []string{"01-01", "25-12", "29-02", "31-12"}
	for _, d := range valid {
		if err := checkDate(d); err != nil {
			t.Errorf("checkDate(%q) = %v, want nil", d, err)
		}
	}
	invalid := []string{"1-1", "25/12", "32-01", "00-01", "31-02", "25-13", "abcd", "2512"}
	for _, d := range invalid {
		if err := checkDate(d); err == nil {
			t.Errorf("checkDate(%q) = nil, want error", d)
		}
	}
}

// TestParseTheme covers the nested theme block: omitted = nil (defaults),
// mode + dark/light palettes parse through, empty fields fall back to the
// built-in defaults via paletteFor, invalid colors/modes and unknown keys
// (including the old flat shape) are rejected.
func TestParseTheme(t *testing.T) {
	minimal := "images:\n  - id: a\n    sources: a.jpg\n"

	// omitted → nil; paletteFor fills the built-in defaults
	c, err := parseConfig([]byte(minimal))
	if err != nil {
		t.Fatalf("parseConfig(minimal): %v", err)
	}
	if c.Theme != nil {
		t.Errorf("theme = %+v, want nil when omitted", c.Theme)
	}
	e := (Theme{}).paletteFor("dark")
	if e != defaultDarkPalette() {
		t.Errorf("paletteFor(dark) of empty theme = %+v, want %+v", e, defaultDarkPalette())
	}
	e = (Theme{}).paletteFor("light")
	if e != defaultLightPalette() {
		t.Errorf("paletteFor(light) of empty theme = %+v, want %+v", e, defaultLightPalette())
	}

	// explicit mode + partial palettes parse; missing fields get defaults
	c, err = parseConfig([]byte("theme:\n  mode: dark\n  dark:\n    background: \"#101010\"\n  light:\n    accent: \"#ff0000\"\n" + minimal))
	if err != nil {
		t.Fatalf("parseConfig(theme): %v", err)
	}
	if c.Theme == nil {
		t.Fatal("theme = nil, want a parsed theme")
	}
	if c.Theme.Mode != "dark" || c.Theme.Dark.Background != "#101010" || c.Theme.Light.Accent != "#ff0000" {
		t.Errorf("theme = %+v, want mode dark, dark.background #101010, light.accent #ff0000", c.Theme)
	}
	e = c.Theme.paletteFor("dark")
	if e.Background != "#101010" || e.Foreground != defaultDarkPalette().Foreground {
		t.Errorf("paletteFor(dark) = %+v, want #101010 background + default foreground", e)
	}
	e = c.Theme.paletteFor("light")
	if e.Accent != "#ff0000" || e.Background != defaultLightPalette().Background {
		t.Errorf("paletteFor(light) = %+v, want #ff0000 accent + default background", e)
	}

	// invalid mode → error
	if _, err := parseConfig([]byte("theme:\n  mode: sepia\n" + minimal)); err == nil {
		t.Error("parseConfig(theme mode: sepia) = nil error, want mode error")
	} else if !strings.Contains(err.Error(), "theme.mode") {
		t.Errorf("error = %q, want a theme.mode mention", err)
	}

	// invalid hex color → error
	if _, err := parseConfig([]byte("theme:\n  light:\n    background: red\n" + minimal)); err == nil {
		t.Error("parseConfig(theme light background: red) = nil error, want hex-color error")
	} else if !strings.Contains(err.Error(), "hex color") {
		t.Errorf("error = %q, want a hex-color mention", err)
	}

	// unknown theme key → strict-decoding error
	if _, err := parseConfig([]byte("theme:\n  colors: nope\n" + minimal)); err == nil {
		t.Error("parseConfig(theme colors:) = nil error, want unknown-field error")
	} else if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error = %q, want an unknown-field mention", err)
	}

	// unknown palette key → strict-decoding error
	if _, err := parseConfig([]byte("theme:\n  dark:\n    colour: \"#ffffff\"\n" + minimal)); err == nil {
		t.Error("parseConfig(theme.dark.colour) = nil error, want unknown-field error")
	}

	// the old flat theme shape is rejected (breaking change)
	if _, err := parseConfig([]byte("theme:\n  background: \"#101010\"\n" + minimal)); err == nil {
		t.Error("parseConfig(flat theme) = nil error, want unknown-field error")
	}
}

// TestParsePlaceholder covers the global placeholder key.
func TestParsePlaceholder(t *testing.T) {
	minimal := "images:\n  - id: a\n    sources: a.jpg\n"

	// omitted → empty
	c, err := parseConfig([]byte(minimal))
	if err != nil {
		t.Fatalf("parseConfig(minimal): %v", err)
	}
	if c.Placeholder != "" {
		t.Errorf("placeholder = %q, want empty when omitted", c.Placeholder)
	}

	// local path
	c, err = parseConfig([]byte("placeholder: ph.jpg\n" + minimal))
	if err != nil {
		t.Fatalf("parseConfig(placeholder): %v", err)
	}
	if c.Placeholder != "ph.jpg" {
		t.Errorf("placeholder = %q, want ph.jpg", c.Placeholder)
	}

	// remote URL
	c, err = parseConfig([]byte("placeholder: https://example.com/ph.gif\n" + minimal))
	if err != nil {
		t.Fatalf("parseConfig(url placeholder): %v", err)
	}
	if c.Placeholder != "https://example.com/ph.gif" {
		t.Errorf("placeholder = %q, want the URL", c.Placeholder)
	}

	// explicit empty behaves as unset
	c, err = parseConfig([]byte("placeholder: \"\"\n" + minimal))
	if err != nil {
		t.Fatalf("parseConfig(empty placeholder): %v", err)
	}
	if c.Placeholder != "" {
		t.Errorf("placeholder = %q, want unset for explicit empty", c.Placeholder)
	}
}

// TestParseRotationForWeekdays covers the new rotation fields: `for` parses
// through the duration parser and `weekdays` is normalized to lowercase.
func TestParseRotationForWeekdays(t *testing.T) {
	cfg := "images:\n  - id: a\n    sources: a.jpg\n    rotation:\n      type: daily\n      at: \"18:00\"\n      for: 6h\n      weekdays: [Mon, FRI]\n"
	c, err := parseConfig([]byte(cfg))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	r := c.Images[0].Rotation
	if r.Type != "daily" || r.At != "18:00" {
		t.Errorf("rotation = %+v, want daily at 18:00", r)
	}
	if r.For != Duration(6*3600) {
		t.Errorf("for = %d, want 6h in seconds", r.For)
	}
	if len(r.Weekdays) != 2 || r.Weekdays[0] != "mon" || r.Weekdays[1] != "fri" {
		t.Errorf("weekdays = %v, want [mon fri] (lowercased)", r.Weekdays)
	}
}

// TestValidateForWeekdays covers the validation rules for `for` and
// `weekdays`: mutual exclusion with `until`, positive and whole-minute
// bounds, the 24h cap, and the "requires type: daily" family.
func TestValidateForWeekdays(t *testing.T) {
	entry := func(rot Rotation) *Config {
		return &Config{Images: []ImageEntry{{ID: "e", Sources: []string{"/data/a.jpg"}, Rotation: rot}}}
	}
	twoSources := func(rot Rotation) *Config {
		return &Config{Images: []ImageEntry{{ID: "e", Sources: []string{"/data/a.jpg", "/data/b.jpg"}, Rotation: rot}}}
	}
	cases := []struct {
		name    string
		cfg     *Config
		wantErr string // substring; "" = valid
	}{
		{"valid for window", entry(Rotation{Type: "daily", At: "18:00", For: Duration(6 * 3600)}), ""},
		{"valid for + every", twoSources(Rotation{Type: "daily", At: "18:00", For: Duration(6 * 3600), Every: Duration(1800)}), ""},
		{"valid for 24h", entry(Rotation{Type: "daily", At: "09:00", For: Duration(24 * 3600)}), ""},
		{"valid weekdays", entry(Rotation{Type: "daily", At: "09:00", Weekdays: []string{"mon", "fri"}}), ""},
		{"until + for conflict", entry(Rotation{Type: "daily", At: "18:00", Until: "06:00", For: Duration(6 * 3600)}), "mutually exclusive"},
		{"for negative", entry(Rotation{Type: "daily", At: "18:00", For: Duration(-3600)}), "must be positive"},
		{"for not whole minutes", entry(Rotation{Type: "daily", At: "18:00", For: Duration(90)}), "whole number of minutes"},
		{"for over 24h", entry(Rotation{Type: "daily", At: "18:00", For: Duration(25 * 3600)}), "at most 24h"},
		{"for on static", entry(Rotation{Type: "static", For: Duration(3600)}), "require `type: daily`"},
		{"weekdays on interval", twoSources(Rotation{Type: "interval", Every: Duration(1800), Weekdays: []string{"mon"}}), "require `type: daily`"},
		{"weekdays on sequential", twoSources(Rotation{Type: "sequential", Weekdays: []string{"mon"}}), "require `type: daily`"},
		{"bad weekday", entry(Rotation{Type: "daily", At: "09:00", Weekdays: []string{"monday"}}), "must be one of mon..sun"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validateConfig = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateConfig = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want a mention of %q", err, tc.wantErr)
			}
		})
	}
}

// TestConfigWarnings covers the never-wins detection: later always-active
// entries and later daily entries with identical gates (including the
// until-vs-for equivalence) produce warnings; healthy layering does not.
func TestConfigWarnings(t *testing.T) {
	entry := func(id string, rot Rotation) ImageEntry {
		return ImageEntry{ID: id, Sources: []string{"/data/" + id + ".jpg"}, Rotation: rot}
	}
	cases := []struct {
		name  string
		cfg   *Config
		wants []string // substrings; nil = no warnings
	}{
		{
			"layered day is clean",
			&Config{Images: []ImageEntry{
				entry("day", Rotation{Type: "static"}),
				entry("evening", Rotation{Type: "daily", At: "18:00"}),
				entry("night", Rotation{Type: "daily", At: "22:00", Until: "06:00"}),
			}},
			nil,
		},
		{
			"later static shadows",
			&Config{Images: []ImageEntry{
				entry("first", Rotation{Type: "static"}),
				entry("second", Rotation{Type: "static"}),
			}},
			[]string{`entry "first" can never be served: a later entry "second"`},
		},
		{
			"skip does not shadow",
			&Config{Images: []ImageEntry{
				entry("day", Rotation{Type: "static"}),
				{ID: "cam", Sources: []string{"/data/cam.jpg"}, Rotation: Rotation{Type: "static"}, OnError: "skip"},
			}},
			nil,
		},
		{
			"identical daily gate",
			&Config{Images: []ImageEntry{
				entry("a", Rotation{Type: "daily", At: "18:00", Until: "06:00"}),
				entry("b", Rotation{Type: "daily", At: "18:00", Until: "06:00"}),
			}},
			[]string{`entry "a" can never be served: a later entry "b"`},
		},
		{
			"until equals for",
			&Config{Images: []ImageEntry{
				entry("a", Rotation{Type: "daily", At: "09:00", Until: "17:00"}),
				entry("b", Rotation{Type: "daily", At: "09:00", For: Duration(8 * 3600)}),
			}},
			[]string{`entry "a" can never be served: a later entry "b"`},
		},
		{
			"different windows no warning",
			&Config{Images: []ImageEntry{
				entry("a", Rotation{Type: "daily", At: "09:00"}),
				entry("b", Rotation{Type: "daily", At: "18:00"}),
			}},
			nil,
		},
		{
			"all-day daily shadows static",
			&Config{Images: []ImageEntry{
				entry("day", Rotation{Type: "static"}),
				entry("allday", Rotation{Type: "daily", At: "00:00"}),
			}},
			[]string{`entry "day" can never be served: a later entry "allday"`},
		},
		{
			"different dates no warning",
			&Config{Images: []ImageEntry{
				entry("xmas", Rotation{Type: "daily", At: "00:00", Dates: []string{"25-12"}}),
				entry("boxing", Rotation{Type: "daily", At: "00:00", Dates: []string{"26-12"}}),
			}},
			nil,
		},
		{
			"same dates warn",
			&Config{Images: []ImageEntry{
				entry("xmas", Rotation{Type: "daily", At: "00:00", Dates: []string{"25-12"}}),
				entry("xmas2", Rotation{Type: "daily", At: "00:00", Dates: []string{"25-12"}}),
			}},
			[]string{`entry "xmas" can never be served: a later entry "xmas2"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := configWarnings(tc.cfg)
			if tc.wants == nil {
				if len(got) != 0 {
					t.Errorf("warnings = %v, want none", got)
				}
				return
			}
			if len(got) != len(tc.wants) {
				t.Fatalf("warnings = %v, want %d: %v", got, len(tc.wants), tc.wants)
			}
			for i, want := range tc.wants {
				if !strings.Contains(got[i], want) {
					t.Errorf("warning[%d] = %q, want a mention of %q", i, got[i], want)
				}
			}
		})
	}
}

// TestValidateAtUntilNonDaily covers H1: `at`/`until` on a non-daily rotation
// type must be rejected (they would otherwise be silently ignored, making an
// entry always-active when the user meant a time gate).
func TestValidateAtUntilNonDaily(t *testing.T) {
	entry := func(rot Rotation) *Config {
		return &Config{Images: []ImageEntry{{ID: "e", Sources: []string{"/data/a.jpg"}, Rotation: rot}}}
	}
	twoSources := func(rot Rotation) *Config {
		return &Config{Images: []ImageEntry{{ID: "e", Sources: []string{"/data/a.jpg", "/data/b.jpg"}, Rotation: rot}}}
	}
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"static with at", entry(Rotation{Type: "static", At: "18:00"})},
		{"static with until", entry(Rotation{Type: "static", Until: "06:00"})},
		{"omitted type with at", entry(Rotation{At: "18:00"})},
		{"interval with at", twoSources(Rotation{Type: "interval", Every: Duration(1800), At: "18:00"})},
		{"sequential with at", twoSources(Rotation{Type: "sequential", At: "18:00"})},
		{"sequential with until", twoSources(Rotation{Type: "sequential", Until: "06:00"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(tc.cfg)
			if err == nil {
				t.Fatal("validateConfig = nil, want an at/until require daily error")
			}
			if !strings.Contains(err.Error(), "`at`/`until` require `type: daily`") {
				t.Errorf("error = %q, want a mention of the require-daily rule", err)
			}
		})
	}
}

// TestValidateDailyRequiresAtHint covers M1: the missing-`at` error teaches
// the all-day encoding.
func TestValidateDailyRequiresAtHint(t *testing.T) {
	c := &Config{Images: []ImageEntry{
		{ID: "e", Sources: []string{"/data/a.jpg"}, Rotation: Rotation{Type: "daily", Dates: []string{"25-12"}}},
	}}
	err := validateConfig(c)
	if err == nil {
		t.Fatal("validateConfig = nil, want a daily-requires-at error")
	}
	if !strings.Contains(err.Error(), "all-day") {
		t.Errorf("error = %q, want the all-day hint", err)
	}
}

// TestParseDurationNode covers the duration parser directly: never, integer
// seconds, Go duration strings, sub-second rejection (B2), invalid strings,
// and overflow.
func TestParseDurationNode(t *testing.T) {
	scalar := func(s string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
	}
	cases := []struct {
		name    string
		in      string
		want    Duration
		wantErr string // substring; "" = success
	}{
		{"never", "never", 0, ""},
		{"integer seconds", "3600", Duration(3600), ""},
		{"zero", "0", 0, ""},
		{"go duration", "30m", Duration(1800), ""},
		{"go duration hours", "6h", Duration(6 * 3600), ""},
		{"sub-second truncates", "500ms", 0, "whole number of seconds"},
		{"sub-second fractional", "1.5s", 0, "whole number of seconds"},
		{"bogus", "bogus", 0, "invalid duration"},
		{"overflow", "99999999999999999999h", 0, "invalid duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDurationNode(scalar(tc.in))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseDurationNode(%q) = %v, want nil", tc.in, err)
				}
				if got != tc.want {
					t.Errorf("parseDurationNode(%q) = %d, want %d", tc.in, got, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseDurationNode(%q) = nil, want error containing %q", tc.in, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want a mention of %q", err, tc.wantErr)
			}
		})
	}
	// a non-scalar node is rejected
	if _, err := parseDurationNode(&yaml.Node{Kind: yaml.SequenceNode}); err == nil {
		t.Error("parseDurationNode(sequence) = nil, want error")
	}
}

// TestValidateAtUntilFormat covers the invalid-`at`/`until` → "HH:MM" error
// path, which is only reachable through validateConfig.
func TestValidateAtUntilFormat(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
	}{
		{"at not hhmm", "at", "6:00pm"},
		{"at 24 hour", "at", "24:00"},
		{"until not hhmm", "until", "noon"},
		{"until trailing space", "until", " 06:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rot := Rotation{Type: "daily", At: "09:00", Until: "17:00"}
			if tc.field == "at" {
				rot.At = tc.value
			} else {
				rot.Until = tc.value
			}
			err := validateConfig(&Config{Images: []ImageEntry{
				{ID: "e", Sources: []string{"/data/a.jpg"}, Rotation: rot},
			}})
			if err == nil {
				t.Fatal("validateConfig = nil, want a HH:MM error")
			}
			if !strings.Contains(err.Error(), "\"HH:MM\"") {
				t.Errorf("error = %q, want a HH:MM mention", err)
			}
		})
	}
}

// TestParseStrictness covers the strict-decoding guarantees: unknown fields at
// the top level and inside images, duplicate keys, and non-string list entries.
func TestParseStrictness(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string // substring
	}{
		{"top-level unknown field", "bogus: 1\nimages:\n  - id: a\n    sources: a.jpg\n", "unknown field"},
		{"entry unknown field", "images:\n  - id: a\n    sources: a.jpg\n    bogus: 1\n", "unknown field"},
		{"rotation unknown field", "images:\n  - id: a\n    sources: a.jpg\n    rotation:\n      type: daily\n      at: \"09:00\"\n      bogus: 1\n", "unknown field"},
		{"duplicate key", "images:\n  - id: a\n    id: b\n    sources: a.jpg\n", "duplicate field"},
		{"non-string sources entry", "images:\n  - id: a\n    sources: [a.jpg, [b.jpg]]\n", "must be strings"},
		{"mapping sources", "images:\n  - id: a\n    sources: {}\n", "must be a string or a list"},
		{"negative every", "images:\n  - id: a\n    sources: [a.jpg, b.jpg]\n    rotation:\n      type: interval\n      every: -30m\n", "`every` must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseConfig([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("parseConfig = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want a mention of %q", err, tc.want)
			}
		})
	}
}
