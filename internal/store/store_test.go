package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/geo"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testFence(t *testing.T, s *Store) Fence {
	t.Helper()
	f := Fence{ID: "home", Name: "home", At: geo.Point{Lat: 59.9139, Lon: 10.7522}, RadiusM: 150}
	if err := s.SaveFence(context.Background(), f); err != nil {
		t.Fatalf("SaveFence: %v", err)
	}
	return f
}

func TestFenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := testFence(t, s)

	got, err := s.Fence(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RadiusM != 150 || got.At.Lat != f.At.Lat {
		t.Errorf("got %+v, want %+v", got, f)
	}

	// Saving again must update rather than fail on the primary key.
	f.RadiusM = 200
	if err := s.SaveFence(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Fence(ctx, f.ID)
	if got.RadiusM != 200 {
		t.Errorf("RadiusM = %d, want 200 after update", got.RadiusM)
	}

	all, err := s.Fences(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("Fences = %v (%v), want 1", all, err)
	}
}

func TestSamplesRoundTripAndAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := testFence(t, s)
	at := time.Unix(1787652000, 0).UTC()

	if err := s.RecordSample(ctx, f.ID, at, "ryde", 3); err != nil {
		t.Fatal(err)
	}
	// Re-recording the same tick must not error - a retried poll is normal.
	if err := s.RecordSample(ctx, f.ID, at, "ryde", 4); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSample(ctx, f.ID, at, "voi", 9); err != nil {
		t.Fatal(err)
	}

	got, err := s.Samples(ctx, f.ID, at.Add(-time.Minute), at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d samples, want 2: %+v", len(got), got)
	}
	for _, p := range got {
		if p.Operator == "ryde" && p.Count != 4 {
			t.Errorf("ryde count = %d, want 4 (the re-record should win)", p.Count)
		}
	}
}

func TestObservePresenceReportsOnlyNewArrivals(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := testFence(t, s)
	t0 := time.Unix(1787652000, 0).UTC()

	seen := []Sighting{{VehicleID: "a", Operator: "ryde"}, {VehicleID: "b", Operator: "voi"}}
	arr, err := s.ObservePresence(ctx, f.ID, t0, seen, DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 2 {
		t.Fatalf("first observation: %d arrivals, want 2", len(arr))
	}

	// The same vehicles a tick later are not new.
	arr, err = s.ObservePresence(ctx, f.ID, t0.Add(30*time.Second), seen, DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 0 {
		t.Errorf("second observation: %d arrivals, want 0: %+v", len(arr), arr)
	}

	// A genuinely new vehicle is an arrival, and only it.
	seen = append(seen, Sighting{VehicleID: "c", Operator: "bolt"})
	arr, err = s.ObservePresence(ctx, f.ID, t0.Add(60*time.Second), seen, DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 || arr[0].VehicleID != "c" {
		t.Errorf("got %+v, want only c", arr)
	}
}

// A vehicle can blink out of the feed for a tick. Without tolerance that blink
// would be reported as a scooter arriving, which is the false alarm that would
// make the whole feature untrustworthy.
func TestObservePresenceToleratesAShortDisappearance(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := testFence(t, s)
	t0 := time.Unix(1787652000, 0).UTC()
	a := []Sighting{{VehicleID: "a", Operator: "ryde"}}

	if _, err := s.ObservePresence(ctx, f.ID, t0, a, DefaultStale); err != nil {
		t.Fatal(err)
	}
	// Missing for one tick...
	if _, err := s.ObservePresence(ctx, f.ID, t0.Add(30*time.Second), nil, DefaultStale); err != nil {
		t.Fatal(err)
	}
	// ...then back, well inside the tolerance.
	arr, err := s.ObservePresence(ctx, f.ID, t0.Add(60*time.Second), a, DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 0 {
		t.Errorf("a blink was reported as an arrival: %+v", arr)
	}

	// Gone long enough, though, and coming back really is an arrival.
	arr, err = s.ObservePresence(ctx, f.ID, t0.Add(60*time.Second).Add(2*DefaultStale), a, DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Errorf("got %d arrivals after a long absence, want 1", len(arr))
	}
}

func TestArrivalsAndPresentIDs(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := testFence(t, s)
	t0 := time.Unix(1787652000, 0).UTC()

	s.ObservePresence(ctx, f.ID, t0, []Sighting{{VehicleID: "a", Operator: "ryde"}}, DefaultStale)
	s.ObservePresence(ctx, f.ID, t0.Add(30*time.Second),
		[]Sighting{{VehicleID: "a", Operator: "ryde"}, {VehicleID: "b", Operator: "voi"}}, DefaultStale)

	arr, err := s.Arrivals(ctx, f.ID, t0.Add(time.Second), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 || arr[0].VehicleID != "b" {
		t.Errorf("Arrivals = %+v, want only b", arr)
	}

	ids, err := s.PresentIDs(ctx, f.ID, t0.Add(30*time.Second), DefaultStale)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("PresentIDs = %v, want 2", ids)
	}
}

func newWatch(id string, expires time.Time) *Watch {
	return &Watch{
		ID: id, Device: "phone", Kind: KindAppearance, FenceID: "home",
		OperatorKeys: []string{"ryde"}, Baseline: []string{"x", "y"},
		CreatedAt: time.Unix(1787652000, 0).UTC(), ExpiresAt: expires,
	}
}

func TestWatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	w := newWatch("w1", now.Add(30*time.Minute))
	if err := s.CreateWatch(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := s.Watch(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateArmed {
		t.Errorf("State = %q, want armed", got.State)
	}
	if len(got.Baseline) != 2 || got.Baseline[0] != "x" {
		t.Errorf("Baseline = %v, want [x y]", got.Baseline)
	}
	if len(got.OperatorKeys) != 1 || got.OperatorKeys[0] != "ryde" {
		t.Errorf("OperatorKeys = %v, want [ryde]", got.OperatorKeys)
	}

	if _, err := s.Watch(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestArmedWatchesExcludesExpiredAndTerminal(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	s.CreateWatch(ctx, newWatch("live", now.Add(30*time.Minute)))
	s.CreateWatch(ctx, newWatch("stale", now.Add(-time.Minute)))
	cancelled := newWatch("cancelled", now.Add(30*time.Minute))
	s.CreateWatch(ctx, cancelled)
	if err := s.SetState(ctx, "cancelled", StateCancelled); err != nil {
		t.Fatal(err)
	}

	armed, err := s.ArmedWatches(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(armed) != 1 || armed[0].ID != "live" {
		ids := make([]string, len(armed))
		for i, w := range armed {
			ids[i] = w.ID
		}
		t.Errorf("armed = %v, want [live]", ids)
	}
}

// A non-repeating watch must disarm in the same statement that records the
// fire, so two overlapping ticks cannot both notify.
func TestMarkFiredDisarmsOnceOnly(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()
	s.CreateWatch(ctx, newWatch("w1", now.Add(30*time.Minute)))

	if err := s.MarkFired(ctx, "w1", now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Watch(ctx, "w1")
	if got.State != StateFired {
		t.Errorf("State = %q, want fired", got.State)
	}
	if got.FiredAt == nil {
		t.Error("FiredAt not recorded")
	}
	if err := s.MarkFired(ctx, "w1", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("second fire returned %v, want ErrNotFound", err)
	}
}

func TestMarkFiredKeepsRepeatingWatchArmed(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()
	w := newWatch("w1", now.Add(30*time.Minute))
	w.Repeat = true
	s.CreateWatch(ctx, w)

	if err := s.MarkFired(ctx, "w1", now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Watch(ctx, "w1")
	if got.State != StateArmed {
		t.Errorf("State = %q, want still armed for a repeating watch", got.State)
	}
	if err := s.MarkFired(ctx, "w1", now.Add(time.Minute)); err != nil {
		t.Errorf("repeating watch could not fire twice: %v", err)
	}
}

func TestExpireWatches(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()
	s.CreateWatch(ctx, newWatch("old", now.Add(-time.Second)))
	s.CreateWatch(ctx, newWatch("new", now.Add(time.Hour)))

	n, err := s.ExpireWatches(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expired %d, want 1", n)
	}
	got, _ := s.Watch(ctx, "old")
	if got.State != StateExpired {
		t.Errorf("State = %q, want expired", got.State)
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()
	s.CreateWatch(ctx, newWatch("w1", now.Add(time.Hour)))

	if err := s.RecordEvent(ctx, "w1", now, []byte(`{"vehicles":1}`)); err != nil {
		t.Fatal(err)
	}
	ev, err := s.Events(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || string(ev[0].Payload) != `{"vehicles":1}` {
		t.Errorf("Events = %+v", ev)
	}
}

// --- outage detection ------------------------------------------------------

func recordNearestAt(t *testing.T, s *Store, at time.Time, op string, dist *int) {
	t.Helper()
	if err := s.RecordNearest(context.Background(), "home", at,
		Nearest{Operator: op, At: at, DistanceM: dist}); err != nil {
		t.Fatal(err)
	}
}

func intp(v int) *int { return &v }

func TestDarkOperatorsFlagsAnOperatorThatStopsPublishing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	// Voi was publishing plenty, then stopped. Bolt is unaffected.
	recordNearestAt(t, s, now.Add(-time.Hour), "voi", intp(80))
	recordNearestAt(t, s, now.Add(-30*time.Minute), "voi", intp(90))
	recordNearestAt(t, s, now, "voi", nil)
	recordNearestAt(t, s, now, "bolt", intp(50))

	dark, err := s.DarkOperators(ctx, "home", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dark) != 1 || dark[0] != "voi" {
		t.Errorf("dark = %v, want [voi]", dark)
	}
}

// Dott has no vehicles in Oslo and never has. That is not a fault, and
// reporting it as one every twenty seconds would make the signal worthless.
func TestDarkOperatorsIgnoresAnOperatorThatNeverServedThisPlace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	recordNearestAt(t, s, now.Add(-time.Hour), "dott", nil)
	recordNearestAt(t, s, now, "dott", nil)
	recordNearestAt(t, s, now, "bolt", intp(50))

	dark, err := s.DarkOperators(ctx, "home", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dark) != 0 {
		t.Errorf("dark = %v, want none - dott simply does not operate here", dark)
	}
}

// Norway's scooter services close overnight. Everything going quiet at once is
// a closed service, not several simultaneous faults.
func TestDarkOperatorsStaysSilentWhenEveryoneIsQuiet(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	for _, op := range []string{"ryde", "voi", "bolt"} {
		recordNearestAt(t, s, now.Add(-time.Hour), op, intp(60))
		recordNearestAt(t, s, now, op, nil)
	}
	dark, err := s.DarkOperators(ctx, "home", now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(dark) != 0 {
		t.Errorf("dark = %v, want none during a service-wide shutdown", dark)
	}
}

func TestDarkOperatorsForgetsStaleHistory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	testFence(t, s)
	now := time.Unix(1787652000, 0).UTC()

	// Present yesterday, absent since. Beyond the lookback this is an
	// operator that has left, not one that broke a moment ago.
	recordNearestAt(t, s, now.Add(-24*time.Hour), "voi", intp(70))
	recordNearestAt(t, s, now, "voi", nil)
	recordNearestAt(t, s, now, "bolt", intp(50))

	dark, err := s.DarkOperators(ctx, "home", now, 6*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(dark) != 0 {
		t.Errorf("dark = %v, want none once the history is stale", dark)
	}
}
