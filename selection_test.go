package main

// Unit tests for the image-selection logic (SPEC §5): entryActive,
// currentSource, and selectEntry. Timestamps are fixed UTC instants (daily
// windows are date-agnostic, so the 1970-01-01 epoch is fine). Most cases are
// I/O-free — sources are plain paths that are never fetched, because
// selectEntry only calls getImageBytes for candidates with on_error:"skip".
// The one I/O-touching path (skip fallback) lives in TestSelectEntrySkipFallback,
// which uses t.TempDir() files and keeps every source path unique per subtest.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEntryActive(t *testing.T) {
	static := ImageEntry{
		ID:       "static",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "static"},
	}
	omitted := ImageEntry{ID: "omitted", Sources: []string{"/data/b.jpg"}} // no rotation block
	interval := ImageEntry{
		ID:       "interval",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg"},
		Rotation: Rotation{Type: "interval", Every: Duration(1800)},
	}
	evening := ImageEntry{ // daily, at 18:00, until defaults to "00:00"
		ID:       "evening",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00"},
	}
	lunch := ImageEntry{ // daily 10:00–14:00, no wrap
		ID:       "lunch",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "daily", At: "10:00", Until: "14:00"},
	}
	night := ImageEntry{ // daily 22:00–06:00, wraps past midnight
		ID:       "night",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"},
	}
	allday := ImageEntry{ // at == until → full 24h
		ID:       "allday",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "daily", At: "12:00", Until: "12:00"},
	}

	cases := []struct {
		name  string
		entry ImageEntry
		now   time.Time
		want  bool
	}{
		// static (and omitted rotation) → always active
		{"static midnight", static, hm(0, 0), true},
		{"static noon", static, hm(12, 0), true},
		{"static 23:59", static, hm(23, 59), true},
		{"omitted rotation midnight", omitted, hm(0, 0), true},
		{"omitted rotation noon", omitted, hm(12, 0), true},
		{"omitted rotation 23:59", omitted, hm(23, 59), true},
		// interval → always active
		{"interval midnight", interval, hm(0, 0), true},
		{"interval noon", interval, hm(12, 0), true},
		{"interval 23:59", interval, hm(23, 59), true},
		// daily basic window: 18:00–00:00 (until defaults to midnight)
		{"daily 17:59 before start", evening, hm(17, 59), false},
		{"daily 18:00 at start", evening, hm(18, 0), true},
		{"daily 23:59 inside", evening, hm(23, 59), true},
		{"daily 00:00 at end", evening, hm(0, 0), false},
		{"daily 09:00 morning", evening, hm(9, 0), false},
		// daily explicit window: 10:00–14:00
		{"daily 09:59 before start", lunch, hm(9, 59), false},
		{"daily 10:00 at start", lunch, hm(10, 0), true},
		{"daily 13:59 inside", lunch, hm(13, 59), true},
		{"daily 14:00 at end", lunch, hm(14, 0), false},
		// daily wrap: 22:00–06:00
		{"wrap 22:00 at start", night, hm(22, 0), true},
		{"wrap 23:59 inside", night, hm(23, 59), true},
		{"wrap 00:00 after midnight", night, hm(0, 0), true},
		{"wrap 05:59 before end", night, hm(5, 59), true},
		{"wrap 06:00 at end", night, hm(6, 0), false},
		{"wrap 21:59 before start", night, hm(21, 59), false},
		{"wrap 12:00 midday", night, hm(12, 0), false},
		// at == until → full 24h
		{"allday 00:00", allday, hm(0, 0), true},
		{"allday 11:59", allday, hm(11, 59), true},
		{"allday 12:00", allday, hm(12, 0), true},
		{"allday 23:59", allday, hm(23, 59), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryActive(tc.entry, tc.now, time.UTC); got != tc.want {
				t.Errorf("entryActive(%s) = %v, want %v", tc.now.Format("15:04"), got, tc.want)
			}
		})
	}
}

// TestEntryActiveTZ checks that the daily window follows the wall clock of the
// target timezone, including the DST offset shift (Berlin: UTC+1 in winter,
// UTC+2 in summer).
func TestEntryActiveTZ(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	evening := ImageEntry{ // daily 18:00–00:00
		ID:       "evening",
		Sources:  []string{"/data/a.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00"},
	}
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		// winter (UTC+1): 16:59 UTC = 17:59 Berlin → before start
		{"winter 16:59 UTC", time.Date(2026, 1, 15, 16, 59, 0, 0, time.UTC), false},
		// winter (UTC+1): 17:00 UTC = 18:00 Berlin → at start
		{"winter 17:00 UTC", time.Date(2026, 1, 15, 17, 0, 0, 0, time.UTC), true},
		// summer (UTC+2): 15:59 UTC = 17:59 Berlin → before start
		{"summer 15:59 UTC", time.Date(2026, 7, 15, 15, 59, 0, 0, time.UTC), false},
		// summer (UTC+2): 16:00 UTC = 18:00 Berlin → at start
		{"summer 16:00 UTC", time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryActive(evening, tc.now, berlin); got != tc.want {
				t.Errorf("entryActive(%s) = %v, want %v", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestEntryActiveDates checks the date gate: `dates` restricts a daily entry
// to specific calendar days, with or without an additional time window.
func TestEntryActiveDates(t *testing.T) {
	xmas := ImageEntry{ // all day on 25-12 (at == until "00:00" → full 24h)
		ID:       "xmas",
		Sources:  []string{"/data/xmas.jpg"},
		Rotation: Rotation{Type: "daily", Dates: []string{"25-12"}, At: "00:00"},
	}
	xmasMorning := ImageEntry{ // 25-12, 00:00–12:00 only
		ID:       "xmas-morning",
		Sources:  []string{"/data/morning.jpg"},
		Rotation: Rotation{Type: "daily", Dates: []string{"25-12"}, At: "00:00", Until: "12:00"},
	}
	newYear := ImageEntry{ // two dates
		ID:       "nye",
		Sources:  []string{"/data/nye.jpg"},
		Rotation: Rotation{Type: "daily", Dates: []string{"31-12", "01-01"}, At: "00:00"},
	}
	at := func(m, d, h, min int) time.Time {
		return time.Date(2026, time.Month(m), d, h, min, 0, 0, time.UTC)
	}
	cases := []struct {
		name  string
		entry ImageEntry
		now   time.Time
		want  bool
	}{
		{"xmas 24-12 18:00", xmas, at(12, 24, 18, 0), false},
		{"xmas 25-12 00:00", xmas, at(12, 25, 0, 0), true},
		{"xmas 25-12 23:59", xmas, at(12, 25, 23, 59), true},
		{"xmas 26-12 00:00", xmas, at(12, 26, 0, 0), false},
		{"xmas-morning 25-12 06:00", xmasMorning, at(12, 25, 6, 0), true},
		{"xmas-morning 25-12 12:00", xmasMorning, at(12, 25, 12, 0), false},
		{"xmas-morning 25-12 18:00", xmasMorning, at(12, 25, 18, 0), false},
		{"xmas-morning 26-12 06:00", xmasMorning, at(12, 26, 6, 0), false},
		{"nye 31-12 20:00", newYear, at(12, 31, 20, 0), true},
		{"nye 01-01 01:00", newYear, at(1, 1, 1, 0), true},
		{"nye 02-01 12:00", newYear, at(1, 2, 12, 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := entryActive(tc.entry, tc.now, time.UTC); got != tc.want {
				t.Errorf("entryActive(%s) = %v, want %v", tc.now.Format("2006-01-02 15:04"), got, tc.want)
			}
		})
	}
}

func TestCurrentSource(t *testing.T) {
	interval2 := ImageEntry{
		ID:       "i2",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg"},
		Rotation: Rotation{Type: "interval", Every: Duration(1800)},
	}
	dayLong := ImageEntry{ // large every, crossing a day boundary
		ID:       "day",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg"},
		Rotation: Rotation{Type: "interval", Every: Duration(86400)},
	}
	three := ImageEntry{
		ID:       "three",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg", "/data/c.jpg"},
		Rotation: Rotation{Type: "interval", Every: Duration(3600)},
	}
	staticMulti := ImageEntry{ // no every → sources[0] even with several sources
		ID:       "multi",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg"},
		Rotation: Rotation{Type: "static"},
	}
	omitted := ImageEntry{ID: "plain", Sources: []string{"/data/a.jpg"}}
	dailyCycle := ImageEntry{ // daily + every: same index math while active
		ID:       "eve",
		Sources:  []string{"/data/sun/a.jpg", "/data/sun/b.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00", Every: Duration(1800)},
	}
	dailyPlain := ImageEntry{ // daily without every → sources[0]
		ID:       "night",
		Sources:  []string{"/data/night.jpg"},
		Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"},
	}

	cases := []struct {
		name  string
		entry ImageEntry
		now   time.Time
		want  string
	}{
		{"static no every", staticMulti, hm(12, 0), "/data/a.jpg"},
		{"omitted rotation", omitted, hm(12, 0), "/data/a.jpg"},
		// epoch-aligned interval: index = (unix / every) % len(sources)
		{"interval unix 0", interval2, time.Unix(0, 0).UTC(), "/data/a.jpg"},
		{"interval unix 1799", interval2, time.Unix(1799, 0).UTC(), "/data/a.jpg"},
		{"interval unix 1800", interval2, time.Unix(1800, 0).UTC(), "/data/b.jpg"},
		{"interval unix 3599", interval2, time.Unix(3599, 0).UTC(), "/data/b.jpg"},
		{"interval unix 3600", interval2, time.Unix(3600, 0).UTC(), "/data/a.jpg"},
		{"interval unix 5400", interval2, time.Unix(5400, 0).UTC(), "/data/b.jpg"},
		// larger every (1 day), crossing a day boundary
		{"long unix 0", dayLong, time.Unix(0, 0).UTC(), "/data/a.jpg"},
		{"long unix 86399", dayLong, time.Unix(86399, 0).UTC(), "/data/a.jpg"},
		{"long unix 86400", dayLong, time.Unix(86400, 0).UTC(), "/data/b.jpg"},
		{"long unix 172799", dayLong, time.Unix(172799, 0).UTC(), "/data/b.jpg"},
		{"long unix 172800", dayLong, time.Unix(172800, 0).UTC(), "/data/a.jpg"},
		// 3 sources
		{"3src unix 0", three, time.Unix(0, 0).UTC(), "/data/a.jpg"},
		{"3src unix 3600", three, time.Unix(3600, 0).UTC(), "/data/b.jpg"},
		{"3src unix 5400", three, time.Unix(5400, 0).UTC(), "/data/b.jpg"},
		{"3src unix 7200", three, time.Unix(7200, 0).UTC(), "/data/c.jpg"},
		{"3src unix 10800", three, time.Unix(10800, 0).UTC(), "/data/a.jpg"},
		// daily + every: cycles within the window (same index math)
		{"daily+every at start", dailyCycle, hm(18, 0), "/data/sun/a.jpg"},
		{"daily+every 18:30", dailyCycle, hm(18, 30), "/data/sun/b.jpg"},
		{"daily+every 19:00", dailyCycle, hm(19, 0), "/data/sun/a.jpg"},
		{"daily no every", dailyPlain, hm(23, 0), "/data/night.jpg"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := currentSource(tc.entry, tc.now); got != tc.want {
				t.Errorf("currentSource(%s) = %q, want %q", tc.now.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestSelectEntry(t *testing.T) {
	staticDay := ImageEntry{ID: "day", Sources: []string{"/data/day.jpg"}}
	staticEve := ImageEntry{ID: "evening", Sources: []string{"/data/evening.jpg"}}
	night := ImageEntry{ // daily 22:00–06:00, wraps midnight
		ID:       "night",
		Sources:  []string{"/data/night.jpg"},
		Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"},
	}
	cycle := ImageEntry{ // daily 18:00–00:00 with source cycling
		ID:       "cycle",
		Sources:  []string{"/data/cycle/a.jpg", "/data/cycle/b.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00", Every: Duration(1800)},
	}

	cases := []struct {
		name      string
		images    []ImageEntry
		id        string
		now       time.Time
		wantID    string
		wantSrc   string
		wantError bool
	}{
		// file order = priority: last active entry wins
		{
			name:    "last active wins",
			images:  []ImageEntry{staticDay, staticEve},
			now:     hm(12, 0),
			wantID:  "evening",
			wantSrc: "/data/evening.jpg",
		},
		// inactive later entries are skipped
		{
			name:    "inactive daily after static",
			images:  []ImageEntry{staticDay, night},
			now:     hm(12, 0), // outside night's window
			wantID:  "day",
			wantSrc: "/data/day.jpg",
		},
		// active later entries override earlier ones
		{
			name:    "active daily after static",
			images:  []ImageEntry{staticDay, night},
			now:     hm(23, 0), // inside night's window
			wantID:  "night",
			wantSrc: "/data/night.jpg",
		},
		// no active entry → error
		{
			name:      "no active entry",
			images:    []ImageEntry{night},
			now:       hm(12, 0),
			wantError: true,
		},
		{
			name:      "empty images",
			images:    []ImageEntry{},
			now:       hm(12, 0),
			wantError: true,
		},
		// ?id= bypass: time gate ignored, cycle still advances
		{
			name:    "id bypass inactive gate",
			images:  []ImageEntry{staticDay, cycle},
			id:      "cycle",
			now:     hm(12, 0), // outside cycle's 18:00 window
			wantID:  "cycle",
			wantSrc: "/data/cycle/a.jpg", // (43200/1800)%2 = 0
		},
		{
			name:    "id bypass cycle advances",
			images:  []ImageEntry{staticDay, cycle},
			id:      "cycle",
			now:     hm(12, 30), // still outside the window
			wantID:  "cycle",
			wantSrc: "/data/cycle/b.jpg", // (45000/1800)%2 = 1
		},
		// unknown id → error
		{
			name:      "unknown id",
			images:    []ImageEntry{staticDay},
			id:        "nope",
			now:       hm(12, 0),
			wantError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setConfig(t, &Config{Images: tc.images})
			gotEntry, gotSrc, err := selectEntry(tc.now, tc.id)
			if tc.wantError {
				if err == nil {
					t.Fatalf("selectEntry(%s, %q) = (%q, %q, nil), want error",
						tc.now.Format(time.RFC3339), tc.id, gotEntry.ID, gotSrc)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectEntry(%s, %q) unexpected error: %v",
					tc.now.Format(time.RFC3339), tc.id, err)
			}
			if gotEntry.ID != tc.wantID {
				t.Errorf("selectEntry id = %q, want %q", gotEntry.ID, tc.wantID)
			}
			if gotSrc != tc.wantSrc {
				t.Errorf("selectEntry source = %q, want %q", gotSrc, tc.wantSrc)
			}
		})
	}
}

// TestSelectEntrySkipFallback exercises the on_error:"skip" path of the walk
// (SPEC §5.2): an entry whose fetch fails counts as inactive for this request,
// so the walk lands on the previous active entry. This is the only part of the
// selection logic that touches real bytes, so every subtest builds its files
// in its own t.TempDir() — each source path is unique and imageCache is reset
// per subtest, keeping them fully independent.
func TestSelectEntrySkipFallback(t *testing.T) {
	writeFile := func(t *testing.T, dir, name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("fake jpeg bytes"), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%s): %v", p, err)
		}
		return p
	}
	missingPath := func(t *testing.T, dir, name string) string {
		t.Helper()
		return filepath.Join(dir, name) // never written, so reads must fail
	}

	cases := []struct {
		name      string
		setup     func(t *testing.T) ([]ImageEntry, string) // images + winning source path
		now       time.Time
		wantID    string
		wantError bool
	}{
		{
			// static first, then a skip entry whose source is missing: the
			// fetch fails, the skip entry counts as inactive, static wins.
			name: "failing skip falls back to earlier static",
			setup: func(t *testing.T) ([]ImageEntry, string) {
				dir := t.TempDir()
				day := writeFile(t, dir, "day.jpg")
				return []ImageEntry{
					{ID: "day", Sources: []string{day}},
					{ID: "cam", Sources: []string{missingPath(t, dir, "cam.jpg")}, OnError: "skip"},
				}, day
			},
			now:    hm(12, 0),
			wantID: "day",
		},
		{
			// the skip entry's file exists, so the fetch succeeds: the entry
			// is active and, as the last active entry, it wins.
			name: "servable skip entry wins",
			setup: func(t *testing.T) ([]ImageEntry, string) {
				dir := t.TempDir()
				day := writeFile(t, dir, "day.jpg")
				cam := writeFile(t, dir, "cam.jpg")
				return []ImageEntry{
					{ID: "day", Sources: []string{day}},
					{ID: "cam", Sources: []string{cam}, OnError: "skip"},
				}, cam
			},
			now:    hm(12, 0),
			wantID: "cam",
		},
		{
			// only a failing skip entry: nothing is servable → error.
			name: "only failing skip entry errors",
			setup: func(t *testing.T) ([]ImageEntry, string) {
				dir := t.TempDir()
				return []ImageEntry{
					{ID: "cam", Sources: []string{missingPath(t, dir, "cam.jpg")}, OnError: "skip"},
				}, ""
			},
			now:       hm(12, 0),
			wantError: true,
		},
		{
			// the walk continues past a failing skip entry: a later active
			// entry still wins.
			name: "later entry wins after failing skip",
			setup: func(t *testing.T) ([]ImageEntry, string) {
				dir := t.TempDir()
				eve := writeFile(t, dir, "eve.jpg")
				return []ImageEntry{
					{ID: "cam", Sources: []string{missingPath(t, dir, "cam.jpg")}, OnError: "skip"},
					{ID: "eve", Sources: []string{eve}},
				}, eve
			},
			now:    hm(12, 0),
			wantID: "eve",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetImageCache(t)
			images, wantSrc := tc.setup(t)
			setConfig(t, &Config{Images: images})
			gotEntry, gotSrc, err := selectEntry(tc.now, "")
			if tc.wantError {
				if err == nil {
					t.Fatalf("selectEntry(%s) = (%q, %q, nil), want error",
						tc.now.Format(time.RFC3339), gotEntry.ID, gotSrc)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectEntry(%s) unexpected error: %v", tc.now.Format(time.RFC3339), err)
			}
			if gotEntry.ID != tc.wantID {
				t.Errorf("selectEntry id = %q, want %q", gotEntry.ID, tc.wantID)
			}
			if gotSrc != wantSrc {
				t.Errorf("selectEntry source = %q, want %q", gotSrc, wantSrc)
			}
		})
	}
}

// TestSequentialSource checks the sequential rotation cursor: advancing calls
// move one step per call and wrap around the source list, starting at index 0;
// a peek does not move the cursor.
func TestSequentialSource(t *testing.T) {
	mu.Lock()
	old := seqIdx
	seqIdx = make(map[string]int64)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		seqIdx = old
		mu.Unlock()
	})

	e := ImageEntry{ID: "seq", Sources: []string{"a", "b", "c"}}
	if got := sequentialSource(e, false); got != "a" {
		t.Fatalf("peek before any advance = %q, want %q", got, "a")
	}
	var got []string
	for i := 0; i < 8; i++ {
		got = append(got, sequentialSource(e, true))
	}
	want := []string{"a", "b", "c", "a", "b", "c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	if peek := sequentialSource(e, false); peek != "c" {
		t.Fatalf("peek after 8 advances = %q, want %q", peek, "c")
	}
}

// ---------------------------------------------------------------------------
// Timeline (admin UI): conditionText, fmtDur, servedAt, nextSlot/prevSlot
// ---------------------------------------------------------------------------

func TestConditionText(t *testing.T) {
	cases := []struct {
		name  string
		entry ImageEntry
		want  string
	}{
		{"static explicit", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}, Rotation: Rotation{Type: "static"}}, "static"},
		{"static omitted", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}}, "static"},
		{"interval", ImageEntry{ID: "a", Sources: []string{"/x.jpg", "/y.jpg"}, Rotation: Rotation{Type: "interval", Every: Duration(1800)}}, "interval · every 30m"},
		{"sequential", ImageEntry{ID: "a", Sources: []string{"/x.jpg", "/y.jpg"}, Rotation: Rotation{Type: "sequential"}}, "sequential"},
		{"daily default until", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}, Rotation: Rotation{Type: "daily", At: "18:00"}}, "daily 18:00–00:00"},
		{"daily wrap", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}, Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"}}, "daily 22:00–06:00"},
		{"daily all day", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}, Rotation: Rotation{Type: "daily", At: "00:00", Until: "00:00"}}, "daily all day"},
		{"daily + every", ImageEntry{ID: "a", Sources: []string{"/x.jpg", "/y.jpg"}, Rotation: Rotation{Type: "daily", At: "18:00", Every: Duration(3600)}}, "daily 18:00–00:00 · every 1h"},
		{"daily + dates", ImageEntry{ID: "a", Sources: []string{"/x.jpg"}, Rotation: Rotation{Type: "daily", At: "00:00", Dates: []string{"25-12", "01-01"}}}, "daily all day · 25-12,01-01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conditionText(tc.entry); got != tc.want {
				t.Errorf("conditionText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		d    Duration
		want string
	}{
		{0, "never"},
		{-1, "never"},
		{45, "45s"},
		{90, "1m"},
		{1800, "30m"},
		{3600, "1h"},
		{5400, "1h30m"},
		{86400, "1d"},
		{172800, "2d"},
	}
	for _, tc := range cases {
		if got := fmtDur(tc.d); got != tc.want {
			t.Errorf("fmtDur(%d) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestBuildTimeline exercises the carousel slots and the next-change scheduler
// against fixed instants.
func TestBuildTimeline(t *testing.T) {
	day := ImageEntry{ID: "day", Sources: []string{"/data/day.jpg"}}
	night := ImageEntry{ // daily 22:00–06:00, wraps midnight
		ID:       "night",
		Sources:  []string{"/data/night.jpg"},
		Rotation: Rotation{Type: "daily", At: "22:00", Until: "06:00"},
	}
	cycle := ImageEntry{ // interval, 2 sources, every 30m
		ID:       "cycle",
		Sources:  []string{"/data/a.jpg", "/data/b.jpg"},
		Rotation: Rotation{Type: "interval", Every: Duration(1800)},
	}
	evening := ImageEntry{ // daily 18:00–00:00 + every 30m
		ID:       "eve",
		Sources:  []string{"/data/sun/a.jpg", "/data/sun/b.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00", Every: Duration(1800)},
	}
	xmas := ImageEntry{ // date-gated, all day on 25-12
		ID:       "xmas",
		Sources:  []string{"/data/xmas.jpg"},
		Rotation: Rotation{Type: "daily", At: "00:00", Dates: []string{"25-12"}},
	}
	seq := ImageEntry{ // sequential — no wall-clock change can be scheduled
		ID:       "seq",
		Sources:  []string{"/data/s1.jpg", "/data/s2.jpg"},
		Rotation: Rotation{Type: "sequential"},
	}

	cases := []struct {
		name        string
		images      []ImageEntry
		now         time.Time
		wantCurID   string
		wantCurSrc  string
		wantPrevID  string
		wantPrevSrc string
		wantNextID  string
		wantNextSrc string
		wantChange  *TimelineChange // nil fields checked only when non-nil
	}{
		{
			// nothing changed within 48h: all slots equal the current state
			name:        "static only",
			images:      []ImageEntry{day},
			now:         hm(12, 0),
			wantCurID:   "day",
			wantCurSrc:  "/data/day.jpg",
			wantPrevID:  "day",
			wantPrevSrc: "/data/day.jpg",
			wantNextID:  "day",
			wantNextSrc: "/data/day.jpg",
			wantChange:  nil,
		},
		{
			// midday: day wins; the image before it was night (22:00–06:00
			// last night); the next change is night's window start at 22:00.
			name:        "daily takeover prev/next",
			images:      []ImageEntry{day, night},
			now:         hm(12, 0),
			wantCurID:   "day",
			wantCurSrc:  "/data/day.jpg",
			wantPrevID:  "night",
			wantPrevSrc: "/data/night.jpg",
			wantNextID:  "night",
			wantNextSrc: "/data/night.jpg",
			wantChange:  &TimelineChange{InSeconds: 10 * 3600, EntryID: "night"},
		},
		{
			// inside the window at 23:00: night wins, prev is day, the next
			// change is the window end at 06:00 tomorrow (back to day).
			name:        "daily takeover inside window",
			images:      []ImageEntry{day, night},
			now:         hm(23, 0),
			wantCurID:   "night",
			wantCurSrc:  "/data/night.jpg",
			wantPrevID:  "day",
			wantPrevSrc: "/data/day.jpg",
			wantNextID:  "day",
			wantNextSrc: "/data/day.jpg",
			wantChange:  &TimelineChange{InSeconds: 7 * 3600, EntryID: "day"},
		},
		{
			// interval cycle at 12:07 (unix 43620): index 24%2=0 → a.jpg.
			// Prev boundary 12:00 (idx 24 → a, unchanged), then 11:30
			// (idx 23 → b). Next boundary 12:30 (idx 25 → b) in 23m.
			name:        "interval cycle",
			images:      []ImageEntry{cycle},
			now:         hm(12, 7),
			wantCurID:   "cycle",
			wantCurSrc:  "/data/a.jpg",
			wantPrevID:  "cycle",
			wantPrevSrc: "/data/b.jpg",
			wantNextID:  "cycle",
			wantNextSrc: "/data/b.jpg",
			wantChange:  &TimelineChange{InSeconds: 23 * 60, EntryID: "cycle"},
		},
		{
			// daily + every at 18:07 (unix 65220): idx 36%2=0 → sun/a.jpg.
			// Next boundary 18:30 (idx 37 → sun/b.jpg).
			name:        "daily + every cycle",
			images:      []ImageEntry{day, evening},
			now:         hm(18, 7),
			wantCurID:   "eve",
			wantCurSrc:  "/data/sun/a.jpg",
			wantPrevID:  "day", // before 18:00, day was served
			wantPrevSrc: "/data/day.jpg",
			wantNextID:  "eve",
			wantNextSrc: "/data/sun/b.jpg",
			wantChange:  &TimelineChange{InSeconds: 23 * 60, EntryID: "eve"},
		},
		{
			// no active entry now (only a future daily entry): next change is
			// the window start tonight; the last image served was night.
			name:        "no active entry",
			images:      []ImageEntry{night},
			now:         hm(12, 0),
			wantCurID:   "",
			wantCurSrc:  "",
			wantPrevID:  "night",
			wantPrevSrc: "/data/night.jpg",
			wantNextID:  "night",
			wantNextSrc: "/data/night.jpg",
			wantChange:  &TimelineChange{InSeconds: 10 * 3600, EntryID: "night"},
		},
		{
			// date-gated entry whose next day is far away (>48h): nothing
			// schedulable, next == current.
			name:        "dates gate far away",
			images:      []ImageEntry{xmas},
			now:         time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			wantCurID:   "",
			wantCurSrc:  "",
			wantPrevID:  "",
			wantPrevSrc: "",
			wantNextID:  "",
			wantNextSrc: "",
			wantChange:  nil,
		},
		{
			// the gated day is within the horizon: tomorrow 00:00 it flips.
			name:        "dates gate within horizon",
			images:      []ImageEntry{xmas},
			now:         time.Date(2026, 12, 24, 12, 0, 0, 0, time.UTC),
			wantCurID:   "",
			wantCurSrc:  "",
			wantPrevID:  "",
			wantPrevSrc: "",
			wantNextID:  "xmas",
			wantNextSrc: "/data/xmas.jpg",
			wantChange:  &TimelineChange{InSeconds: 12 * 3600, EntryID: "xmas"},
		},
		{
			// sequential: no wall-clock change can be scheduled.
			name:        "sequential no schedule",
			images:      []ImageEntry{seq},
			now:         hm(12, 0),
			wantCurID:   "seq",
			wantCurSrc:  "/data/s1.jpg",
			wantPrevID:  "seq",
			wantPrevSrc: "/data/s1.jpg",
			wantNextID:  "seq",
			wantNextSrc: "/data/s1.jpg",
			wantChange:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Images: tc.images}
			tl := buildTimeline(c, tc.now, time.UTC)
			if tl.Current.EntryID != tc.wantCurID || tl.Current.Source != tc.wantCurSrc {
				t.Errorf("current = (%q, %q), want (%q, %q)",
					tl.Current.EntryID, tl.Current.Source, tc.wantCurID, tc.wantCurSrc)
			}
			if tl.Prev.EntryID != tc.wantPrevID || tl.Prev.Source != tc.wantPrevSrc {
				t.Errorf("prev = (%q, %q), want (%q, %q)",
					tl.Prev.EntryID, tl.Prev.Source, tc.wantPrevID, tc.wantPrevSrc)
			}
			if tl.Next.EntryID != tc.wantNextID || tl.Next.Source != tc.wantNextSrc {
				t.Errorf("next = (%q, %q), want (%q, %q)",
					tl.Next.EntryID, tl.Next.Source, tc.wantNextID, tc.wantNextSrc)
			}
			if tc.wantChange == nil {
				if tl.NextChange != nil {
					t.Errorf("next_change = %+v, want nil", tl.NextChange)
				}
				return
			}
			if tl.NextChange == nil {
				t.Fatalf("next_change = nil, want %+v", tc.wantChange)
			}
			if tl.NextChange.InSeconds != tc.wantChange.InSeconds {
				t.Errorf("next_change.in_seconds = %d, want %d",
					tl.NextChange.InSeconds, tc.wantChange.InSeconds)
			}
			if tl.NextChange.EntryID != tc.wantChange.EntryID {
				t.Errorf("next_change.entry_id = %q, want %q",
					tl.NextChange.EntryID, tc.wantChange.EntryID)
			}
			// the change's state must match the next slot it produces
			if tl.NextChange.Source != tl.Next.Source || tl.NextChange.Condition != tl.Next.Condition {
				t.Errorf("next_change (%q, %q) does not match next slot (%q, %q)",
					tl.NextChange.Source, tl.NextChange.Condition, tl.Next.Source, tl.Next.Condition)
			}
			// entries list sanity: every entry appears, winner is flagged
			if len(tl.Entries) != len(tc.images) {
				t.Fatalf("entries = %d, want %d", len(tl.Entries), len(tc.images))
			}
			for i, e := range tl.Entries {
				if e.ID != tc.images[i].ID {
					t.Errorf("entries[%d].id = %q, want %q", i, e.ID, tc.images[i].ID)
				}
				if e.Condition == "" {
					t.Errorf("entries[%d] (%s): empty condition", i, e.ID)
				}
			}
		})
	}
}

// TestBuildTimelineTZ checks that daily-window candidates follow the
// configured timezone (Berlin winter: UTC+1).
func TestBuildTimelineTZ(t *testing.T) {
	berlin := mustLoc(t, "Europe/Berlin")
	evening := ImageEntry{ // daily 18:00–00:00 Berlin
		ID:       "eve",
		Sources:  []string{"/data/eve.jpg"},
		Rotation: Rotation{Type: "daily", At: "18:00"},
	}
	c := &Config{Images: []ImageEntry{evening}}
	// 16:59 UTC = 17:59 Berlin → inactive; the window opens in 1 minute.
	tl := buildTimeline(c, time.Date(2026, 1, 15, 16, 59, 0, 0, time.UTC), berlin)
	if tl.Current.EntryID != "" {
		t.Errorf("current = %q, want empty (17:59 Berlin is before 18:00)", tl.Current.EntryID)
	}
	if tl.NextChange == nil {
		t.Fatal("next_change = nil, want the 18:00 Berlin window start")
	}
	if tl.NextChange.InSeconds != 60 {
		t.Errorf("next_change.in_seconds = %d, want 60", tl.NextChange.InSeconds)
	}
	if tl.NextChange.EntryID != "eve" {
		t.Errorf("next_change.entry_id = %q, want %q", tl.NextChange.EntryID, "eve")
	}
}
