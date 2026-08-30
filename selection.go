package main

// Image selection logic (§5): pure functions, no I/O, no global state (except
// the sequential cursor, which is inherently in-memory state guarded by mu).

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var errNoEntry = errors.New("no active entry")

// windowOf resolves a daily entry's activation window as minutes since local
// midnight: active iff `start <= mins < start+dur`. `until` and `for` are two
// encodings of the same window: `until` gives dur = (until-start+1440) % 1440
// (with at == until meaning the full 24h), `for` is the explicit length in
// whole minutes (validation enforces both). dur is always > 0 (1440 = all day).
func windowOf(r Rotation) (start, dur int, ok bool) {
	start, ok = parseHM(r.At)
	if !ok {
		return 0, 0, false
	}
	if r.For > 0 {
		dur = int(r.For / 60)
		if dur <= 0 || dur > 1440 {
			return 0, 0, false
		}
		return start, dur, true
	}
	end, ok := parseHM(defaultUntil(r))
	if !ok {
		return 0, 0, false
	}
	dur = (end - start + 1440) % 1440
	if dur == 0 {
		dur = 1440 // at == until → the full 24 hours
	}
	return start, dur, true
}

// weekdayKey renders t's weekday as the lowercase 3-letter `weekdays` gate
// format ("mon", "tue", ...).
func weekdayKey(t time.Time) string {
	return strings.ToLower(t.Weekday().String()[:3])
}

// entryActive reports whether e is active at now (§4.4). The daily time
// window, the date gate, and the weekday gate are evaluated against the wall
// clock in loc (the configured timezone), so `at`/`until`/`dates`/`weekdays`
// mean what they say on a local clock. DST edge days follow wall-clock
// semantics: a spring-forward hour never matches, a repeated fall-back hour
// matches twice.
func entryActive(e ImageEntry, now time.Time, loc *time.Location) bool {
	if e.Rotation.Type != "daily" {
		return true // static, interval and sequential are always active
	}
	start, dur, ok := windowOf(e.Rotation)
	if !ok {
		return false
	}
	local := now.In(loc)
	if len(e.Rotation.Dates) > 0 && !containsStr(e.Rotation.Dates, dateKey(local)) {
		return false
	}
	if len(e.Rotation.Weekdays) > 0 && !containsStr(e.Rotation.Weekdays, weekdayKey(local)) {
		return false
	}
	mins := local.Hour()*60 + local.Minute()
	end := start + dur
	if end > 1440 {
		return mins >= start || mins < end-1440 // window wraps past midnight
	}
	return mins >= start && mins < end
}

// currentSource picks the source e should show at now (§5.3).
func currentSource(e ImageEntry, now time.Time) string {
	n := len(e.Sources)
	if n == 0 {
		return ""
	}
	if e.Rotation.Every > 0 {
		idx := (now.Unix() / int64(e.Rotation.Every)) % int64(n)
		if idx < 0 {
			idx += int64(n)
		}
		return e.Sources[idx]
	}
	return e.Sources[0]
}

// sequentialSource returns the source at the current sequential cursor for e.
// When advance is true the cursor is bumped one step (wrapping around the
// source list); otherwise it is only read (id-preview). The first GET after
// startup/config reload serves sources[0].
func sequentialSource(e ImageEntry, advance bool) string {
	i := sequentialCursor(e)
	if advance {
		mu.Lock()
		seqIdx[e.ID] = i + 1
		mu.Unlock()
	}
	return e.Sources[int(i%int64(len(e.Sources)))]
}

// sequentialCursor returns the sequential cursor position for e without
// advancing it (a read-only peek takes only the read lock).
func sequentialCursor(e ImageEntry) int64 {
	mu.RLock()
	i := seqIdx[e.ID]
	mu.RUnlock()
	return i
}

// selectEntry walks the config top to bottom and returns the last active entry
// and its current source. With id != "" the time gate is ignored (§5). It
// copies the config under RLock and releases before any I/O, so the
// on_error:"skip" fetch check can never deadlock against the cache lock.
func selectEntry(now time.Time, id string) (ImageEntry, string, error) {
	mu.RLock()
	c := config
	loc := timeLoc
	mu.RUnlock()
	if c == nil {
		return ImageEntry{}, "", errNoEntry
	}
	if id != "" {
		for i := range c.Images {
			if c.Images[i].ID == id {
				e := c.Images[i]
				if e.OnError == "skip" {
					e.OnError = "placeholder" // no meaningful fallback in id mode
				}
				return e, currentSource(e, now), nil
			}
		}
		return ImageEntry{}, "", errNoEntry
	}
	var winner *ImageEntry
	for i := range c.Images {
		e := &c.Images[i]
		if !entryActive(*e, now, loc) {
			continue
		}
		if e.OnError == "skip" {
			if err := probeSource(currentSource(*e, now)); err != nil {
				continue // fetch failed → counts as inactive for this request
			}
		}
		winner = e
	}
	if winner == nil {
		return ImageEntry{}, "", errNoEntry
	}
	return *winner, currentSource(*winner, now), nil
}

// ---------------------------------------------------------------------------
// Timeline (admin UI: the `timeline` field of /api/status) — pure planning
// over the config
// ---------------------------------------------------------------------------

// TimelineSlot describes one carousel slot (prev/current/next).
type TimelineSlot struct {
	EntryID   string `json:"entry_id"` // "" = no active entry
	Source    string `json:"source"`
	Condition string `json:"condition"`
}

// TimelineEntry is one per-entry row for the timeline strip.
type TimelineEntry struct {
	ID        string `json:"id"`
	Condition string `json:"condition"`
	Active    bool   `json:"active"`    // structurally active at now
	IsWinner  bool   `json:"is_winner"` // served at now
}

// TimelineChange describes the next scheduled change of the served image.
type TimelineChange struct {
	At        string `json:"at"`         // RFC3339 UTC instant of the change
	InSeconds int64  `json:"in_seconds"` // seconds from now (rounded down)
	EntryID   string `json:"entry_id"`   // "" = no active entry afterwards
	Source    string `json:"source"`
	Condition string `json:"condition"`
}

// Timeline is the timeline payload served in /api/status.
type Timeline struct {
	UTCNow       string          `json:"utc_now"`
	Timezone     string          `json:"timezone"`
	ActiveEntry  string          `json:"active_entry_id"`
	ActiveSource string          `json:"active_source"`
	Prev         TimelineSlot    `json:"prev"`
	Current      TimelineSlot    `json:"current"`
	Next         TimelineSlot    `json:"next"`
	NextChange   *TimelineChange `json:"next_change"`
	Entries      []TimelineEntry `json:"entries"`
}

// servedAt evaluates the config structurally at t: walk top to bottom, the
// last active entry wins. Pure — no fetch checks, no global state. (A failing
// on_error:"skip" entry can therefore diverge from what /image actually
// serves; the timeline is a planning aid, and the carousel renders the real
// image through /image?id= anyway.)
func servedAt(c *Config, t time.Time, loc *time.Location) (ImageEntry, string, bool) {
	var winner *ImageEntry
	for i := range c.Images {
		e := &c.Images[i]
		if entryActive(*e, t, loc) {
			winner = e
		}
	}
	if winner == nil {
		return ImageEntry{}, "", false
	}
	return *winner, currentSource(*winner, t), true
}

// conditionText renders the human-readable activation condition of an entry.
func conditionText(e ImageEntry) string {
	r := e.Rotation
	switch r.Type {
	case "", "static":
		return "static"
	case "interval":
		return "interval · every " + fmtDur(r.Every)
	case "sequential":
		return "sequential"
	case "daily":
		var s string
		if r.For > 0 {
			if int(r.For/60) == 1440 {
				s = "daily all day"
			} else {
				s = "daily " + r.At + " for " + fmtDur(r.For)
			}
		} else if r.At == defaultUntil(e.Rotation) {
			s = "daily all day"
		} else {
			s = "daily " + r.At + "–" + defaultUntil(e.Rotation)
		}
		if r.Every > 0 {
			s += " · every " + fmtDur(r.Every)
		}
		if len(r.Dates) > 0 {
			s += " · " + strings.Join(r.Dates, ",")
		}
		if len(r.Weekdays) > 0 {
			s += " · " + strings.Join(r.Weekdays, ",")
		}
		return s
	}
	return "unknown"
}

// fmtDur renders a Duration compactly: "45s", "30m", "1h30m", "2d".
func fmtDur(d Duration) string {
	secs := int64(d)
	switch {
	case secs <= 0:
		return "never"
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	case secs < 86400:
		if secs%3600 == 0 {
			return fmt.Sprintf("%dh", secs/3600)
		}
		return fmt.Sprintf("%dh%dm", secs/3600, (secs%3600)/60)
	default:
		return fmt.Sprintf("%dd", secs/86400)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// defaultUntil resolves the daily window end: an empty `until` means "00:00"
// (midnight); the window wraps past midnight when until <= at.
func defaultUntil(r Rotation) string {
	if r.Until == "" {
		return "00:00"
	}
	return r.Until
}

// dateKey renders t's calendar date as "DD-MM" — the `dates` gate format.
func dateKey(t time.Time) string {
	return fmt.Sprintf("%02d-%02d", t.Day(), int(t.Month()))
}

// candidates returns the instants within 48h of now at which the served image
// may change — daily window starts/ends (respecting the dates gate) and cycle
// boundaries of every>0 entries. future selects (now, now+48h] ascending,
// otherwise [now-48h, now) descending; both are deduped.
func candidates(c *Config, now time.Time, loc *time.Location, future bool) []time.Time {
	horizon := now.Add(48 * time.Hour)
	if !future {
		horizon = now.Add(-48 * time.Hour)
	}
	set := make(map[int64]bool)
	add := func(t time.Time) {
		if future && t.After(now) && !t.After(horizon) {
			set[t.Unix()] = true
		}
		if !future && t.Before(now) && !t.Before(horizon) {
			set[t.Unix()] = true
		}
	}
	local := now.In(loc)
	for i := range c.Images {
		e := &c.Images[i]
		if e.Rotation.Type == "daily" {
			start, dur, ok := windowOf(e.Rotation)
			if !ok {
				continue
			}
			lo, hi := 0, 2 // today .. day+2 covers any 48h window
			if !future {
				lo, hi = -2, 0 // today .. day-2
			}
			for d := lo; d <= hi; d++ {
				day := local.AddDate(0, 0, d)
				if len(e.Rotation.Dates) > 0 && !containsStr(e.Rotation.Dates, dateKey(day)) {
					continue
				}
				if len(e.Rotation.Weekdays) > 0 && !containsStr(e.Rotation.Weekdays, weekdayKey(day)) {
					continue
				}
				yy, mm, dd := day.Date()
				startAt := time.Date(yy, mm, dd, start/60, start%60, 0, 0, loc)
				add(startAt)
				// The window end is start+dur minutes — for a wrap (end >
				// 1440) this lands on the next calendar day, which
				// time.Date(yy, mm, dd, end/60, ...) could not express.
				add(startAt.Add(time.Duration(dur) * time.Minute))
			}
		}
		if e.Rotation.Every > 0 {
			step := int64(e.Rotation.Every)
			first := now.Unix()/step*step + step // first boundary strictly after now
			inc := step
			if !future {
				first = now.Unix()/step*step - step // first boundary strictly before now
				inc = -step
			}
			for t := time.Unix(first, 0).UTC(); (future && !t.After(horizon)) || (!future && !t.Before(horizon)); t = t.Add(time.Duration(inc) * time.Second) {
				add(t)
			}
		}
	}
	out := make([]int64, 0, len(set))
	for u := range set {
		out = append(out, u)
	}
	if future {
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	} else {
		sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	}
	times := make([]time.Time, len(out))
	for i, u := range out {
		times[i] = time.Unix(u, 0).UTC()
	}
	return times
}

// slotOf renders the served state (or the no-active-entry placeholder) as a
// carousel slot.
func slotOf(e ImageEntry, src string, ok bool) TimelineSlot {
	if !ok {
		return TimelineSlot{EntryID: "", Source: "", Condition: "no active entry"}
	}
	return TimelineSlot{EntryID: e.ID, Source: src, Condition: conditionText(e)}
}

// prevSlot finds the state served just before the most recent change (within
// 48h). When nothing changed, it equals the current state.
func prevSlot(c *Config, now time.Time, loc *time.Location) TimelineSlot {
	curE, curSrc, curOK := servedAt(c, now, loc)
	for _, t := range candidates(c, now, loc, false) {
		e, src, ok := servedAt(c, t, loc)
		if ok != curOK || e.ID != curE.ID || src != curSrc {
			return slotOf(e, src, ok)
		}
	}
	return slotOf(curE, curSrc, curOK)
}

// nextSlot finds the first future change of the served image (within 48h) and
// the state served after it. When nothing is scheduled, next == current and
// the change is nil.
func nextSlot(c *Config, now time.Time, loc *time.Location) (TimelineSlot, *TimelineChange) {
	curE, curSrc, curOK := servedAt(c, now, loc)
	for _, t := range candidates(c, now, loc, true) {
		e, src, ok := servedAt(c, t, loc)
		if ok != curOK || e.ID != curE.ID || src != curSrc {
			slot := slotOf(e, src, ok)
			return slot, &TimelineChange{
				At:        t.UTC().Format(time.RFC3339),
				InSeconds: t.Unix() - now.Unix(),
				EntryID:   slot.EntryID,
				Source:    slot.Source,
				Condition: slot.Condition,
			}
		}
	}
	return slotOf(curE, curSrc, curOK), nil
}

// buildTimeline assembles the admin-UI timeline: per-entry conditions, the
// prev/current/next carousel slots, and the next scheduled change. Structural
// evaluation only (no fetch checks) — pure and table-testable.
func buildTimeline(c *Config, now time.Time, loc *time.Location) Timeline {
	t := Timeline{
		UTCNow:   now.UTC().Format(time.RFC3339),
		Timezone: loc.String(),
	}
	curE, curSrc, curOK := servedAt(c, now, loc)
	if curOK {
		t.ActiveEntry = curE.ID
		t.ActiveSource = curSrc
	}
	t.Current = slotOf(curE, curSrc, curOK)
	t.Prev = prevSlot(c, now, loc)
	t.Next, t.NextChange = nextSlot(c, now, loc)
	for i := range c.Images {
		e := &c.Images[i]
		t.Entries = append(t.Entries, TimelineEntry{
			ID:        e.ID,
			Condition: conditionText(*e),
			Active:    entryActive(*e, now, loc),
			IsWinner:  curOK && e.ID == curE.ID,
		})
	}
	return t
}

// sequentialSlots overrides the structural carousel slots for a sequential
// entry with its in-memory cursor state (§5.3): the current slot is the image
// /image serves right now (the cursor position — peeking never advances),
// next is the following request's image, prev the preceding one.
func sequentialSlots(e ImageEntry, cond string) (prev, cur, next TimelineSlot) {
	n := len(e.Sources)
	if n == 0 {
		return TimelineSlot{}, TimelineSlot{EntryID: e.ID, Condition: cond}, TimelineSlot{}
	}
	slot := func(i int) TimelineSlot {
		return TimelineSlot{EntryID: e.ID, Source: e.Sources[((i%n)+n)%n], Condition: cond}
	}
	idx := int(sequentialCursor(e))
	// A dashboard GET /image serves sources[cursor] and THEN advances, so the
	// image /image just served is one step behind the peek: current = last
	// served (cursor-1), next = the upcoming image the next GET will serve
	// (cursor), prev = the one before (cursor-2).
	return slot(idx - 2), slot(idx - 1), slot(idx)
}
