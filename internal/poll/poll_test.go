package poll

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/store"
)

var home = geo.Point{Lat: 59.9139, Lon: 10.7522}

// fakeFetcher returns a scripted answer and records what was asked.
type fakeFetcher struct {
	vehicles  []entur.Vehicle
	queries   []entur.Query
	err       error
	truncated bool
}

func (f *fakeFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return &entur.Result{Vehicles: f.vehicles, Returned: len(f.vehicles), Truncated: f.truncated}, nil
}

// near returns a point roughly metres north of home.
func near(metresNorth float64) geo.Point {
	return geo.Point{Lat: home.Lat + metresNorth/111320.0, Lon: home.Lon}
}

func vehicle(id, operator string, metresNorth float64, rangeM int) entur.Vehicle {
	return entur.Vehicle{
		ID: id, OperatorKey: operator, Operator: operator,
		At: near(metresNorth), RangeM: rangeM, FormFactor: entur.ScooterStanding,
	}
}

type harness struct {
	p     *Poller
	fetch *fakeFetcher
	st    *store.Store
	sent  []Notification
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{fetch: &fakeFetcher{}, st: st}
	h.p = &Poller{
		Client: h.fetch,
		Store:  st,
		Sink: SinkFunc(func(ctx context.Context, n Notification) error {
			h.sent = append(h.sent, n)
			return nil
		}),
	}
	if err := st.SaveFence(context.Background(), store.Fence{
		ID: "home", Name: "home", At: home, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) arm(t *testing.T, w *store.Watch) {
	t.Helper()
	if err := h.st.CreateWatch(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func appearanceWatch(id string, baseline []string, ops []string, now time.Time) *store.Watch {
	return &store.Watch{
		ID: id, Device: "phone", Kind: store.KindAppearance, FenceID: "home",
		OperatorKeys: ops, Baseline: baseline,
		CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute),
	}
}

var t0 = time.Unix(1787652000, 0).UTC()

func TestAppearanceWatchIgnoresTheBaseline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// The scooter was already there when the watch was armed.
	h.fetch.vehicles = []entur.Vehicle{vehicle("a", "ryde", 50, 20000)}
	h.arm(t, appearanceWatch("w1", []string{"a"}, []string{"ryde"}, t0))

	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fired) != 0 {
		t.Errorf("fired %v on a baseline vehicle; a watch armed when one is already there must not fire instantly", rep.Fired)
	}
	if len(h.sent) != 0 {
		t.Errorf("notified %d times, want 0", len(h.sent))
	}
}

func TestAppearanceWatchFiresOnceOnANewVehicle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.arm(t, appearanceWatch("w1", nil, []string{"ryde"}, t0))

	// Nothing there yet.
	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fired) != 0 {
		t.Fatalf("fired on an empty fence: %v", rep.Fired)
	}

	// One shows up.
	h.fetch.vehicles = []entur.Vehicle{vehicle("new", "ryde", 60, 20000)}
	rep, err = h.p.Tick(ctx, t0.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fired) != 1 || rep.Fired[0] != "w1" {
		t.Fatalf("Fired = %v, want [w1]", rep.Fired)
	}
	if len(h.sent) != 1 {
		t.Fatalf("notified %d times, want 1", len(h.sent))
	}
	if len(h.sent[0].Vehicles) != 1 || h.sent[0].Vehicles[0].ID != "new" {
		t.Errorf("notification carried %+v, want the new vehicle", h.sent[0].Vehicles)
	}

	// It is still there on the next tick, but the watch has disarmed.
	rep, err = h.p.Tick(ctx, t0.Add(40*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fired) != 0 {
		t.Errorf("fired again after disarming: %v", rep.Fired)
	}
	if len(h.sent) != 1 {
		t.Errorf("notified %d times total, want 1", len(h.sent))
	}
}

func TestAppearanceWatchRespectsOperatorFilter(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.arm(t, appearanceWatch("w1", nil, []string{"ryde"}, t0))

	// A Voi turns up. If your subscription only pays for Ryde, this is not
	// the notification you asked for.
	h.fetch.vehicles = []entur.Vehicle{vehicle("v", "voi", 40, 20000)}
	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Fired) != 0 {
		t.Errorf("a Voi fired a Ryde-only watch")
	}
}

func TestAppearanceWatchRespectsMinRange(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	w := appearanceWatch("w1", nil, []string{"ryde"}, t0)
	w.MinRangeM = 10000
	h.arm(t, w)

	h.fetch.vehicles = []entur.Vehicle{vehicle("flat", "ryde", 40, 3000)}
	rep, _ := h.p.Tick(ctx, t0)
	if len(rep.Fired) != 0 {
		t.Errorf("a nearly flat scooter fired a watch with a range floor")
	}

	h.fetch.vehicles = append(h.fetch.vehicles, vehicle("full", "ryde", 45, 25000))
	rep, _ = h.p.Tick(ctx, t0.Add(20*time.Second))
	if len(rep.Fired) != 1 {
		t.Errorf("Fired = %v, want the watch to fire on the usable one", rep.Fired)
	}
}

func TestVehiclesOutsideTheFenceDoNotFire(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.arm(t, appearanceWatch("w1", nil, []string{"ryde"}, t0))

	// 400 m north, well outside the 150 m fence, but inside the coalesced
	// query radius - exactly the case client-side trimming exists for.
	h.fetch.vehicles = []entur.Vehicle{vehicle("far", "ryde", 400, 20000)}
	rep, _ := h.p.Tick(ctx, t0)
	if len(rep.Fired) != 0 {
		t.Errorf("a vehicle outside the fence fired the watch")
	}
}

func TestScarcityWatchFiresAtOrBelowThreshold(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	w := &store.Watch{
		ID: "s1", Device: "phone", Kind: store.KindScarcity, FenceID: "home",
		OperatorKeys: []string{"ryde"}, Threshold: 2,
		CreatedAt: t0, ExpiresAt: t0.Add(time.Hour),
	}
	h.arm(t, w)

	h.fetch.vehicles = []entur.Vehicle{
		vehicle("a", "ryde", 30, 20000),
		vehicle("b", "ryde", 40, 20000),
		vehicle("c", "ryde", 50, 20000),
	}
	rep, _ := h.p.Tick(ctx, t0)
	if len(rep.Fired) != 0 {
		t.Fatalf("fired at 3 with a threshold of 2")
	}

	h.fetch.vehicles = h.fetch.vehicles[:2]
	rep, _ = h.p.Tick(ctx, t0.Add(20*time.Second))
	if len(rep.Fired) != 1 {
		t.Errorf("Fired = %v, want the scarcity watch to fire at 2", rep.Fired)
	}
}

func TestExpiredWatchesNeitherFireNorLinger(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.arm(t, appearanceWatch("w1", nil, []string{"ryde"}, t0))
	h.fetch.vehicles = []entur.Vehicle{vehicle("new", "ryde", 50, 20000)}

	later := t0.Add(31 * time.Minute)
	rep, err := h.p.Tick(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Expired != 1 {
		t.Errorf("Expired = %d, want 1", rep.Expired)
	}
	if len(rep.Fired) != 0 {
		t.Errorf("an expired watch fired: %v", rep.Fired)
	}
	got, _ := h.st.Watch(ctx, "w1")
	if got.State != store.StateExpired {
		t.Errorf("State = %q, want expired", got.State)
	}
}

func TestTickRecordsHistoryIncludingZeroes(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.fetch.vehicles = []entur.Vehicle{
		vehicle("a", "ryde", 30, 20000),
		vehicle("b", "voi", 40, 20000),
	}
	if _, err := h.p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}

	got, err := h.st.Samples(ctx, "home", t0.Add(-time.Minute), t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, s := range got {
		counts[s.Operator] = s.Count
	}
	if counts["ryde"] != 1 || counts["voi"] != 1 {
		t.Errorf("counts = %v, want ryde and voi at 1", counts)
	}
	// "No Bolt here" is an observation worth keeping; a missing row would be
	// indistinguishable from a missed poll.
	if n, ok := counts["bolt"]; !ok || n != 0 {
		t.Errorf("bolt = %v (present=%v), want an explicit zero", n, ok)
	}
}

func TestTickRecordsArrivals(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.fetch.vehicles = []entur.Vehicle{vehicle("a", "ryde", 30, 20000)}
	rep, _ := h.p.Tick(ctx, t0)
	if rep.Arrivals != 1 {
		t.Errorf("Arrivals = %d, want 1 on first sighting", rep.Arrivals)
	}
	rep, _ = h.p.Tick(ctx, t0.Add(20*time.Second))
	if rep.Arrivals != 0 {
		t.Errorf("Arrivals = %d, want 0 for a vehicle that never left", rep.Arrivals)
	}
}

func TestNearbyFencesShareOneQuery(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.st.SaveFence(ctx, store.Fence{
		ID: "work", Name: "work", At: near(300), RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queries != 1 {
		t.Errorf("Queries = %d, want 1 - two fences 300 m apart should coalesce", rep.Queries)
	}
	if rep.Fences != 2 {
		t.Errorf("Fences = %d, want 2", rep.Fences)
	}
}

func TestDistantFencesGetTheirOwnQueries(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	// Trondheim: coalescing this with Oslo would mean fetching half of Norway.
	if err := h.st.SaveFence(ctx, store.Fence{
		ID: "trd", Name: "trondheim",
		At: geo.Point{Lat: 63.4305, Lon: 10.3951}, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queries != 2 {
		t.Errorf("Queries = %d, want 2 for fences 400 km apart", rep.Queries)
	}
	// The cap governs coalesced fence queries. Nearest probes are single
	// -operator and deliberately wide, but return only a handful of rows.
	for _, q := range h.fetch.queries {
		if len(q.OperatorKeys) == 1 {
			continue
		}
		if q.RadiusM > MaxQueryRadiusM {
			t.Errorf("fence query radius %d m exceeds the cap", q.RadiusM)
		}
	}
}

// A watch that has already fired and been recorded must stay fired even if the
// notification could not be delivered.
func TestFailedDeliveryDoesNotUnfireTheWatch(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.p.Sink = SinkFunc(func(ctx context.Context, n Notification) error {
		return errors.New("broker unreachable")
	})
	h.arm(t, appearanceWatch("w1", nil, []string{"ryde"}, t0))
	h.fetch.vehicles = []entur.Vehicle{vehicle("new", "ryde", 50, 20000)}

	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatalf("a failed delivery must not fail the tick: %v", err)
	}
	if len(rep.Fired) != 1 {
		t.Fatalf("Fired = %v, want [w1]", rep.Fired)
	}
	got, _ := h.st.Watch(ctx, "w1")
	if got.State != store.StateFired {
		t.Errorf("State = %q, want fired despite the delivery failure", got.State)
	}
	ev, _ := h.st.Events(ctx, "w1")
	if len(ev) != 1 {
		t.Errorf("recorded %d events, want 1", len(ev))
	}
}

func TestUpstreamFailureSurfacesButIsNotFatal(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.fetch.err = errors.New("connection reset")
	if _, err := h.p.Tick(ctx, t0); err == nil {
		t.Error("want an error when upstream fails")
	}
}

func TestTickWithNoFencesDoesNothing(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	defer st.Close()
	p := &Poller{Client: &fakeFetcher{}, Store: st}
	rep, err := p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queries != 0 {
		t.Errorf("Queries = %d, want 0 with no fences", rep.Queries)
	}
}

// --- nearest ---------------------------------------------------------------

// operatorFetcher answers per-operator queries, so the nearest lookup can be
// exercised without a network.
type operatorFetcher struct {
	inFence []entur.Vehicle
	beyond  map[string][]entur.Vehicle
	probes  []entur.Query
}

func (f *operatorFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	if len(q.OperatorKeys) == 1 {
		f.probes = append(f.probes, q)
		vs := f.beyond[q.OperatorKeys[0]]
		return &entur.Result{Vehicles: vs, Returned: len(vs)}, nil
	}
	return &entur.Result{Vehicles: f.inFence, Returned: len(f.inFence)}, nil
}

func newNearestHarness(t *testing.T) (*Poller, *operatorFetcher, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &operatorFetcher{beyond: map[string][]entur.Vehicle{}}
	p := &Poller{Client: f, Store: st}
	if err := st.SaveFence(context.Background(), store.Fence{
		ID: "home", Name: "home", At: home, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	return p, f, st
}

func TestNearestInFenceCostsNoExtraQuery(t *testing.T) {
	ctx := context.Background()
	p, f, st := newNearestHarness(t)
	f.inFence = []entur.Vehicle{
		vehicle("b1", "bolt", 120, 20000),
		vehicle("b2", "bolt", 40, 20000), // the closer Bolt
	}
	rep, err := p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.LatestNearest(ctx, "home")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := got["bolt"]
	if !ok || n.DistanceM == nil {
		t.Fatalf("no nearest recorded for bolt: %+v", got)
	}
	if n.VehicleID != "b2" {
		t.Errorf("nearest bolt = %q, want the closer one b2", n.VehicleID)
	}
	if *n.DistanceM > 45 {
		t.Errorf("distance = %d m, want ~40", *n.DistanceM)
	}
	// Bolt was present, so no probe should have been spent on it.
	for _, q := range f.probes {
		if q.OperatorKeys[0] == "bolt" {
			t.Error("probed for an operator that was already in the fence")
		}
	}
	if rep.NearestQueries == 0 {
		t.Error("expected probes for the absent operators")
	}
}

func TestNearestBeyondTheFenceIsFound(t *testing.T) {
	ctx := context.Background()
	p, f, st := newNearestHarness(t)
	// Nothing in the fence at all, but a Ryde 480 m away.
	f.beyond["ryde"] = []entur.Vehicle{vehicle("far-ryde", "ryde", 480, 30000)}

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LatestNearest(ctx, "home")
	n, ok := got["ryde"]
	if !ok || n.DistanceM == nil {
		t.Fatalf("nearest ryde not recorded: %+v", got)
	}
	if *n.DistanceM < 470 || *n.DistanceM > 490 {
		t.Errorf("distance = %d m, want ~480", *n.DistanceM)
	}
}

// "No Ryde within 5 km" is a different answer from "we did not look", and the
// distinction has to survive into storage.
func TestNearestRecordsAbsenceExplicitly(t *testing.T) {
	ctx := context.Background()
	p, f, st := newNearestHarness(t)
	f.beyond["ryde"] = nil

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	got, _ := st.LatestNearest(ctx, "home")
	n, ok := got["ryde"]
	if !ok {
		t.Fatal("no row recorded for an operator with nothing within reach")
	}
	if n.DistanceM != nil {
		t.Errorf("DistanceM = %v, want nil", *n.DistanceM)
	}
}

// The wider lookup is context for a human, not the thing watches depend on,
// so it runs on its own slower schedule.
func TestNearestLookupIsRateLimited(t *testing.T) {
	ctx := context.Background()
	p, f, _ := newNearestHarness(t)
	p.NearestInterval = 2 * time.Minute

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	first := len(f.probes)
	if first == 0 {
		t.Fatal("no probes on the first tick")
	}
	if _, err := p.Tick(ctx, t0.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(f.probes) != first {
		t.Errorf("probed again after 20s: %d -> %d", first, len(f.probes))
	}
	if _, err := p.Tick(ctx, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(f.probes) == first {
		t.Error("did not probe again after the interval elapsed")
	}
}

func TestNearestCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	p, f, _ := newNearestHarness(t)
	p.NearestInterval = -1

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if len(f.probes) != 0 {
		t.Errorf("probed %d times despite being disabled", len(f.probes))
	}
}

// A probe asks for a margin, not exactly one vehicle: the upstream selects the
// nearest N before we filter out the unrentable, so asking for one and getting
// a disabled scooter would read as "nothing within 5 km".
func TestNearestProbeAsksForAMargin(t *testing.T) {
	ctx := context.Background()
	p, f, _ := newNearestHarness(t)
	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	if len(f.probes) == 0 {
		t.Fatal("no probes made")
	}
	for _, q := range f.probes {
		if q.Limit <= 1 {
			t.Errorf("probe Limit = %d, want a margin above 1", q.Limit)
		}
		if q.RadiusM != DefaultReachM {
			t.Errorf("probe radius = %d, want %d", q.RadiusM, DefaultReachM)
		}
	}
}

// A failing probe is context lost, not a tick lost - the watches must survive.
func TestNearestFailureDoesNotFailTheTick(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	defer st.Close()
	if err := st.SaveFence(ctx, store.Fence{
		ID: "home", Name: "home", At: home, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	f := &failingProbeFetcher{}
	p := &Poller{Client: f, Store: st}
	if _, err := p.Tick(ctx, t0); err != nil {
		t.Errorf("tick failed because a nearest probe did: %v", err)
	}
}

type failingProbeFetcher struct{}

func (f *failingProbeFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	if len(q.OperatorKeys) == 1 {
		return nil, errors.New("probe failed")
	}
	return &entur.Result{}, nil
}

// A coalesced query is centred on a circle covering several fences, so the
// distances the client computed are relative to that centre. Every fence must
// see distances measured from itself, or "61 m west" refers to somewhere
// nobody is standing.
func TestCoalescedQueryDistancesAreRebasedPerFence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if err := h.st.SaveFence(ctx, store.Fence{
		ID: "work", Name: "work", At: near(300), RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	// Sitting right on the "work" fence, 300 m from "home".
	v := vehicle("v", "ryde", 300, 20000)
	v.DistanceM = 999 // whatever the client computed from the query centre
	h.fetch.vehicles = []entur.Vehicle{v}

	rep, err := h.p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Queries != 1 {
		t.Fatalf("expected one coalesced query, got %d", rep.Queries)
	}
	got, err := h.st.LatestNearest(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	n, ok := got["ryde"]
	if !ok || n.DistanceM == nil {
		t.Fatalf("nearest not recorded for work: %+v", got)
	}
	if *n.DistanceM > 20 {
		t.Errorf("distance from work = %d m, want ~0 - it is sitting on the fence", *n.DistanceM)
	}
}
