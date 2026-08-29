package main

// Tests for config parsing/validation helpers (SPEC §4.4, §4.5): resolveTZ
// and checkDate. Full parseConfig coverage is exercised indirectly through
// the /api/config handler tests; the selection-logic tables live in
// selection_test.go.

import (
	"strings"
	"testing"
	"time"
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
