package quiethours

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Settings is a recipient's quiet-hours configuration, mirrored from the
// account record (todo 6: store.User.QuietStart/QuietEnd/QuietTZ). Start/End
// are "HH:MM" in 24-hour form; TZ is an IANA location name. Enabled is false
// when Start or End is empty (the account API clears both together). A caller
// may also force Enabled=false to bypass regardless of the window.
type Settings struct {
	Start   string // "HH:MM", empty when unset
	End     string // "HH:MM", empty when unset
	TZ      string // IANA name, e.g. "America/New_York"; "" treated as UTC
	Enabled bool
}

// SettingsFromAccount maps the account columns onto Settings. The window is
// enabled iff both Start and End are non-empty. It accepts the raw store
// fields by value so this package does not import internal/store (keeps it a
// pure leaf with no non-stdlib dependencies).
func SettingsFromAccount(start, end, tz string) Settings {
	s := Settings{Start: start, End: end, TZ: tz, Enabled: start != "" && end != ""}
	if s.TZ == "" {
		s.TZ = "UTC"
	}
	return s
}

// Decision is the outcome of evaluating one message against a recipient's
// quiet hours.
type Decision int

const (
	// Deliver publishes to live transports immediately (bypass, window
	// disabled, outside the window, or unparseable settings).
	Deliver Decision = iota
	// Hold withholds live delivery until the window ends. The message row is
	// already persisted; only live push is deferred.
	Hold
)

// ErrNotInWindow is returned by Manager.Hold when called outside a quiet
// window. Callers normally gate Hold on Evaluate == Hold, so this only fires
// on a clock/settings race between Evaluate and Hold.
var ErrNotInWindow = errors.New("quiethours: not inside quiet window")

// Evaluate decides whether a message of the given priority should be held for
// the recipient at now. Priority >= 1 always bypasses (Deliver). A disabled,
// empty, or unparseable window also yields Deliver. Otherwise Deliver outside
// the window and Hold inside it.
func Evaluate(now time.Time, s Settings, priority int) Decision {
	if priority >= 1 {
		return Deliver
	}
	if !s.Enabled {
		return Deliver
	}
	in, _, ok := windowContains(now, s)
	if !ok || !in {
		return Deliver
	}
	return Hold
}

// InWindow reports whether now falls inside the quiet window defined by s.
// Unparseable or disabled settings return false.
func InWindow(now time.Time, s Settings) bool {
	in, _, _ := windowContains(now, s)
	return in
}

// ReleaseAt returns the instant at which a hold placed at now should be
// released (the end of the current quiet window). The returned instant is
// strictly after now and is timezone-correct (DST-aware). ok is false when the
// settings are unusable or now is outside the window.
func ReleaseAt(now time.Time, s Settings) (time.Time, bool) {
	in, rel, _ := windowContains(now, s)
	if !in {
		return time.Time{}, false
	}
	return rel, true
}

// windowContains reports whether now is inside the window and, if so, returns
// the window-end release instant. ok is false when the settings are unusable
// (bad TZ, malformed HH:MM, or a zero-length window); inWindow is then false.
// The window is half-open [start, end): the end instant is the release point
// and is itself considered outside the window (deliver/flush), matching the
// manual-QA "clock 07:00 -> released" boundary.
func windowContains(now time.Time, s Settings) (inWindow bool, release time.Time, ok bool) {
	sh, sm, sok := hhmm(s.Start)
	eh, em, eok := hhmm(s.End)
	if !sok || !eok {
		return false, time.Time{}, false
	}
	loc, err := time.LoadLocation(s.TZ)
	if err != nil {
		return false, time.Time{}, false
	}
	// A zero-length window (start == end) holds nothing.
	if sh == eh && sm == em {
		return false, time.Time{}, false
	}
	local := now.In(loc)
	y, mo, d := local.Date()
	startToday := time.Date(y, mo, d, sh, sm, 0, 0, loc)
	endToday := time.Date(y, mo, d, eh, em, 0, 0, loc)
	crosses := mins(sh, sm) > mins(eh, em)
	if !crosses {
		// Same-day window [start, end).
		if !local.Before(startToday) && local.Before(endToday) {
			return true, endToday, true
		}
		return false, time.Time{}, true
	}
	// Overnight window [start, 24:00) ∪ [00:00, end).
	if !local.Before(startToday) {
		// Evening portion: release at tomorrow's end.
		return true, endToday.AddDate(0, 0, 1), true
	}
	if local.Before(endToday) {
		// Morning portion: release at today's end.
		return true, endToday, true
	}
	return false, time.Time{}, true
}

// hhmm parses an "HH:MM" 24-hour string. ok is false on any malformed value
// or out-of-range component.
func hhmm(s string) (hour, minute int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	mi, err := strconv.Atoi(parts[1])
	if err != nil || mi < 0 || mi > 59 {
		return 0, 0, false
	}
	return h, mi, true
}

// mins is the wall-clock minute-of-day for an hour/minute pair.
func mins(h, m int) int { return h*60 + m }
