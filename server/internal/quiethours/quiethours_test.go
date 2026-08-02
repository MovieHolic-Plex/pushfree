package quiethours_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/quiethours"
)

// mustLoc loads an IANA location, failing the test if the platform's tzdata
// lacks it (the DST cases require America/New_York).
func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation %q: %v (tzdata missing?)", name, err)
	}
	return loc
}

// atWall builds an instant from a wall-clock time in loc. Centralised so every
// case uses the injected (deterministic) clock, never time.Now.
func atWall(t *testing.T, loc *time.Location, wall string) time.Time {
	t.Helper()
	got, err := time.ParseInLocation("2006-01-02 15:04:05", wall, loc)
	if err != nil {
		t.Fatalf("ParseInLocation %q: %v", wall, err)
	}
	return got
}

// --- Priority bypass --------------------------------------------------------

func TestEvaluatePriorityBypass(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	now := atWall(t, loc, "2026-07-15 03:00:00") // inside 02:00-07:00
	s := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	cases := []struct {
		priority int
		want     quiethours.Decision
	}{
		{-2, quiethours.Hold},   // lowest -> held
		{-1, quiethours.Hold},   // quiet default -> held
		{0, quiethours.Hold},    // normal -> held
		{1, quiethours.Deliver}, // high -> bypass
		{2, quiethours.Deliver}, // emergency -> bypass (manual-QA failure case)
	}
	for _, c := range cases {
		got := quiethours.Evaluate(now, s, c.priority)
		if got != c.want {
			t.Errorf("priority %d: got %v, want %v", c.priority, got, c.want)
		}
	}
}

// --- In-window hold vs outside-window deliver -------------------------------

func TestEvaluateInsideOutsideWindow(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	inside := []string{"2026-07-15 02:00:00", "2026-07-15 03:00:00", "2026-07-15 06:59:59"}
	for _, wall := range inside {
		now := atWall(t, loc, wall)
		if got := quiethours.Evaluate(now, s, 0); got != quiethours.Hold {
			t.Errorf("inside %s: got %v, want Hold", wall, got)
		}
	}
	outside := []string{"2026-07-15 07:00:00", "2026-07-15 12:00:00", "2026-07-15 01:59:59"}
	for _, wall := range outside {
		now := atWall(t, loc, wall)
		if got := quiethours.Evaluate(now, s, 0); got != quiethours.Deliver {
			t.Errorf("outside %s: got %v, want Deliver", wall, got)
		}
	}
}

// --- Overnight window (crosses midnight) ------------------------------------

func TestOvernightWindow(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s := quiethours.Settings{Start: "22:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	evening := atWall(t, loc, "2026-07-15 23:30:00") // after 22:00
	morning := atWall(t, loc, "2026-07-15 03:00:00") // before 07:00
	midday := atWall(t, loc, "2026-07-15 12:00:00")  // outside

	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"evening", evening, true},
		{"morning", morning, true},
		{"midday", midday, false},
	} {
		if got := quiethours.InWindow(tc.now, s); got != tc.want {
			t.Errorf("overnight %s: InWindow got %v, want %v", tc.name, got, tc.want)
		}
	}

	// Release at the evening position is tomorrow's 07:00; at the morning
	// position it is today's 07:00.
	rel, ok := quiethours.ReleaseAt(evening, s)
	if !ok {
		t.Fatal("evening ReleaseAt: ok=false")
	}
	wantEveningRelease := atWall(t, loc, "2026-07-16 07:00:00")
	if !rel.Equal(wantEveningRelease) {
		t.Errorf("evening release: got %v, want %v", rel, wantEveningRelease)
	}
	rel, ok = quiethours.ReleaseAt(morning, s)
	if !ok {
		t.Fatal("morning ReleaseAt: ok=false")
	}
	wantMorningRelease := atWall(t, loc, "2026-07-15 07:00:00")
	if !rel.Equal(wantMorningRelease) {
		t.Errorf("morning release: got %v, want %v", rel, wantMorningRelease)
	}
}

// --- DST boundary: window spans the US spring-forward transition ------------

func TestDSTSpringForwardSpan(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	// On 2026-03-08 clocks spring 02:00 EST -> 03:00 EDT. A hold placed at
	// 00:30 (EST, UTC-5) inside an overnight 22:00-07:00 window must release
	// at 07:00 EDT (UTC-4): the window end is on the far side of the jump.
	now := atWall(t, loc, "2026-03-08 00:30:00")
	s := quiethours.Settings{Start: "22:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	if !quiethours.InWindow(now, s) {
		t.Fatal("expected in window at 00:30 EST")
	}
	_, offNow := now.Zone()
	if offNow != -5*3600 {
		t.Errorf("now offset: got %d, want EST -18000", offNow)
	}

	rel, ok := quiethours.ReleaseAt(now, s)
	if !ok {
		t.Fatal("ReleaseAt ok=false")
	}
	_, offRel := rel.Zone()
	if offRel != -4*3600 {
		t.Errorf("release offset: got %d, want EDT -14400", offRel)
	}
	wantUTC := time.Date(2026, 3, 8, 11, 0, 0, 0, time.UTC) // 07:00 EDT
	if !rel.Equal(wantUTC) {
		t.Errorf("release UTC: got %v, want %v", rel.UTC(), wantUTC.UTC())
	}
}

// --- DST boundary: same wall-clock window resolves to a different UTC release
// in summer (EDT) vs winter (EST). -------------------------------------------

func TestDSTSummerVsWinter(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	s := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	summer := atWall(t, loc, "2026-07-15 03:00:00") // EDT
	winter := atWall(t, loc, "2026-01-15 03:00:00") // EST

	relSummer, ok := quiethours.ReleaseAt(summer, s)
	if !ok {
		t.Fatal("summer ReleaseAt ok=false")
	}
	relWinter, ok := quiethours.ReleaseAt(winter, s)
	if !ok {
		t.Fatal("winter ReleaseAt ok=false")
	}
	// Same wall-clock end (07:00 local) on both days, but the UTC offset
	// differs by one hour across DST: EDT (-4h) in summer, EST (-5h) in
	// winter. That offset delta is the DST-awareness signal; the UTC instants
	// themselves differ by ~6 months because they are different dates.
	_, offSummer := relSummer.Zone()
	_, offWinter := relWinter.Zone()
	if offSummer != -4*3600 {
		t.Errorf("summer offset: got %d, want EDT -14400", offSummer)
	}
	if offWinter != -5*3600 {
		t.Errorf("winter offset: got %d, want EST -18000", offWinter)
	}
	if offSummer-offWinter != 3600 {
		t.Errorf("offset delta: got %d, want +3600 (DST hour)", offSummer-offWinter)
	}
	yS, mS, dS := relSummer.Date()
	hS, miS, _ := relSummer.Clock()
	yW, mW, dW := relWinter.Date()
	hW, miW, _ := relWinter.Clock()
	if !(hS == 7 && miS == 0 && hW == 7 && miW == 0) {
		t.Errorf("wall-clock release: summer %02d:%02d, winter %02d:%02d, want both 07:00",
			hS, miS, hW, miW)
	}
	if !(yS == 2026 && yW == 2026 && mS == time.July && dS == 15 && mW == time.January && dW == 15) {
		t.Errorf("release dates drifted: summer %v winter %v", relSummer, relWinter)
	}
}

// --- Disabled / unparseable settings never hold -----------------------------

func TestDisabledAndBadSettingsDeliver(t *testing.T) {
	loc := mustLoc(t, "UTC")
	now := atWall(t, loc, "2026-07-15 03:00:00")
	disabled := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "UTC", Enabled: false}
	if got := quiethours.Evaluate(now, disabled, 0); got != quiethours.Deliver {
		t.Errorf("disabled: got %v, want Deliver", got)
	}
	// SettingsFromAccount: empty start/end -> disabled.
	if s := quiethours.SettingsFromAccount("", "", "UTC"); s.Enabled {
		t.Errorf("empty start/end should be disabled")
	}
	bad := []quiethours.Settings{
		{Start: "02:00", End: "07:00", TZ: "Not/A/Zone", Enabled: true}, // bad tz
		{Start: "bad", End: "07:00", TZ: "UTC", Enabled: true},          // bad start
		{Start: "25:00", End: "07:00", TZ: "UTC", Enabled: true},        // out of range
		{Start: "07:00", End: "07:00", TZ: "UTC", Enabled: true},        // zero-length
	}
	for i, s := range bad {
		if got := quiethours.Evaluate(now, s, 0); got != quiethours.Deliver {
			t.Errorf("bad[%d]: got %v, want Deliver", i, got)
		}
	}
}

// --- End-to-end Manager hold + window-end flush -----------------------------

func TestManagerHoldAndRelease(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	settings := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}

	// Mutable clock: tests advance `now` and the Manager observes it through
	// the closure, with no real sleeps.
	now := atWall(t, loc, "2026-07-15 03:00:00")
	clock := func() time.Time { return now }
	mgr := quiethours.New(quiethours.NewMemoryHoldStore(), clock, nil)

	if err := mgr.Hold(context.Background(), 100, 7, settings); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := mgr.Hold(context.Background(), 101, 7, settings); err != nil {
		t.Fatalf("Hold #2: %v", err)
	}

	pending, err := mgr.Pending(context.Background(), 7)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending after hold: got %d, want 2", len(pending))
	}

	// Still before window end: nothing released.
	if now.Before(atWall(t, loc, "2026-07-15 07:00:00")) == false {
		t.Fatal("test clock setup: expected now < 07:00")
	}
	due, err := mgr.ReleaseDue(context.Background())
	if err != nil {
		t.Fatalf("ReleaseDue pre-end: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("pre-end release: got %d, want 0", len(due))
	}

	// Advance the injected clock to exactly the window end: all held messages
	// for the recipient flush.
	now = atWall(t, loc, "2026-07-15 07:00:00")
	due, err = mgr.ReleaseDue(context.Background())
	if err != nil {
		t.Fatalf("ReleaseDue at end: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("at-end release: got %d, want 2", len(due))
	}
	if due[0].MessageID != 100 || due[1].MessageID != 101 {
		t.Errorf("release order: got [%d,%d], want [100,101]", due[0].MessageID, due[1].MessageID)
	}
	// Store is drained.
	pending, _ = mgr.Pending(context.Background(), 7)
	if len(pending) != 0 {
		t.Errorf("post-release pending: got %d, want 0", len(pending))
	}
}

// --- Per-recipient independence: one held, one delivered, separate releases -

func TestManagerPerRecipientIndependence(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	windowed := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}
	always := quiethours.Settings{Start: "", End: "", TZ: "UTC", Enabled: false} // no window

	now := atWall(t, loc, "2026-07-15 03:00:00")
	clock := func() time.Time { return now }
	mgr := quiethours.New(quiethours.NewMemoryHoldStore(), clock, nil)

	// Same send, priority 0, two recipients with different settings.
	for _, tc := range []struct {
		recipient int64
		msgID     int64
		s         quiethours.Settings
	}{
		{7, 200, windowed}, // in window -> hold
		{8, 201, always},   // no window -> deliver (caller does not Hold)
	} {
		if quiethours.Evaluate(mgr.Now(), tc.s, 0) == quiethours.Hold {
			if err := mgr.Hold(context.Background(), tc.msgID, tc.recipient, tc.s); err != nil {
				t.Fatalf("Hold recipient %d: %v", tc.recipient, err)
			}
		}
	}

	p7, _ := mgr.Pending(context.Background(), 7)
	p8, _ := mgr.Pending(context.Background(), 8)
	if len(p7) != 1 || p7[0].MessageID != 200 {
		t.Errorf("recipient 7 pending: got %+v, want [200]", p7)
	}
	if len(p8) != 0 {
		t.Errorf("recipient 8 pending: got %+v, want []", p8)
	}

	// Releasing recipient 7's window does not touch recipient 8 (independent
	// release instants). Here both are flushed at the window end, but only 7
	// had a hold.
	now = atWall(t, loc, "2026-07-15 07:00:00")
	due, err := mgr.ReleaseDue(context.Background())
	if err != nil {
		t.Fatalf("ReleaseDue: %v", err)
	}
	if len(due) != 1 || due[0].RecipientUserID != 7 || due[0].MessageID != 200 {
		t.Errorf("due: got %+v, want recipient=7 msg=200", due)
	}
}

// --- Hold outside the window returns ErrNotInWindow -------------------------

func TestHoldOutsideWindowErrors(t *testing.T) {
	loc := mustLoc(t, "America/New_York")
	now := atWall(t, loc, "2026-07-15 12:00:00") // outside 02:00-07:00
	mgr := quiethours.New(quiethours.NewMemoryHoldStore(), func() time.Time { return now }, nil)
	settings := quiethours.Settings{Start: "02:00", End: "07:00", TZ: "America/New_York", Enabled: true}
	err := mgr.Hold(context.Background(), 1, 1, settings)
	if !errors.Is(err, quiethours.ErrNotInWindow) {
		t.Fatalf("Hold outside window: got %v, want ErrNotInWindow", err)
	}
}

// --- Run flushes past-due holds via the release callback --------------------
//
// Run is the only piece that uses a real ticker. To stay deterministic we add
// a hold whose ReleaseAt is already in the past, then await the release signal
// on a channel with a bounded timeout (subscribe-before-trigger; no blind
// sleep). poll is short so the signal arrives well within the bound.
func TestRunFlushesDueHolds(t *testing.T) {
	store := quiethours.NewMemoryHoldStore()
	now := time.Date(2026, 7, 15, 7, 0, 0, 0, time.UTC)
	mgr := quiethours.New(store, func() time.Time { return now }, nil)
	// Past-due hold added directly to the store (release at 06:00, clock at
	// 07:00) so the very first Run tick finds it due.
	if err := store.Add(context.Background(), quiethours.HeldMessage{
		MessageID:       300,
		RecipientUserID: 9,
		ReleaseAt:       time.Date(2026, 7, 15, 6, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("add past-due hold: %v", err)
	}

	released := make(chan []quiethours.HeldMessage, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.Run(ctx, 5*time.Millisecond, func(due []quiethours.HeldMessage) {
		select {
		case released <- due:
		default:
		}
	})

	select {
	case due := <-released:
		if len(due) != 1 || due[0].MessageID != 300 || due[0].RecipientUserID != 9 {
			t.Fatalf("Run released: %+v, want msg=300 recipient=9", due)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not flush past-due hold within 2s")
	}
}
