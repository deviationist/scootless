package poll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/store"
)

// slowFetcher blocks for delay on every query and records how many calls were
// ever in flight at once, which is what separates "ran in parallel" from "ran
// quickly".
type slowFetcher struct {
	delay time.Duration

	// byFence answers a coalesced query with vehicles keyed on the queried
	// point, so a test can prove a group's result reached the right fence.
	byFence map[string][]entur.Vehicle
	// perOperator answers a single-operator nearest probe.
	perOperator map[string][]entur.Vehicle

	failAt func(q entur.Query) error

	inFlight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
}

func (f *slowFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	n := f.inFlight.Add(1)
	for {
		peak := f.peak.Load()
		if n <= peak || f.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer f.inFlight.Add(-1)
	f.calls.Add(1)

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.failAt != nil {
		if err := f.failAt(q); err != nil {
			return nil, err
		}
	}
	if len(q.OperatorKeys) == 1 {
		vs := f.perOperator[q.OperatorKeys[0]]
		return &entur.Result{Vehicles: vs, Returned: len(vs)}, nil
	}
	vs := f.byFence[key(q.At)]
	return &entur.Result{Vehicles: vs, Returned: len(vs)}, nil
}

func key(p geo.Point) string { return fmt.Sprintf("%.4f,%.4f", p.Lat, p.Lon) }

// farApart returns points spread wider than MaxQueryRadiusM, so groupFences
// gives each fence a query of its own rather than coalescing them.
func farApart(n int) []geo.Point {
	out := make([]geo.Point, n)
	for i := range out {
		// ~5 km apart in latitude, comfortably past the 2 km coalescing cap.
		out[i] = geo.Point{Lat: home.Lat + float64(i)*0.045, Lon: home.Lon}
	}
	return out
}

func fencesAt(t *testing.T, st *store.Store, pts []geo.Point) []store.Fence {
	t.Helper()
	var fs []store.Fence
	for i, p := range pts {
		f := store.Fence{
			ID: fmt.Sprintf("f%d", i), Name: fmt.Sprintf("f%d", i),
			At: p, RadiusM: 150,
		}
		if err := st.SaveFence(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		fs = append(fs, f)
	}
	return fs
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A tick's group queries are independent round trips. Running them in sequence
// made a tick as long as the sum of them, so every fence after the first
// learned about its scooters later than the one before it.
func TestGroupQueriesRunConcurrently(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	pts := farApart(4)
	fencesAt(t, st, pts)

	f := &slowFetcher{delay: 60 * time.Millisecond, byFence: map[string][]entur.Vehicle{}}
	p := &Poller{Client: f, Store: st, NearestInterval: -1}

	start := time.Now()
	rep, err := p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if rep.Queries != 4 {
		t.Fatalf("Queries = %d, want 4 separate groups", rep.Queries)
	}
	if got := f.peak.Load(); got < 4 {
		t.Errorf("peak concurrent queries = %d, want 4; the groups ran in sequence", got)
	}
	// Four 60 ms queries in sequence take 240 ms. Allow generous headroom for
	// a loaded CI box while still failing if they serialised.
	if elapsed > 200*time.Millisecond {
		t.Errorf("tick took %v for 4 x 60ms queries; they did not overlap", elapsed)
	}
}

// The parallel fetch must not shuffle which result belongs to which fence.
// This is the failure mode a concurrent rewrite invites and the one that would
// be hardest to notice: every fence still gets vehicles, just the wrong ones.
func TestEachGroupsResultReachesItsOwnFence(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	pts := farApart(4)
	fences := fencesAt(t, st, pts)

	// Each fence's query answers with one vehicle 10 m north of that fence,
	// named after it. A crossed wire puts a vehicle outside the fence it
	// lands on, so the count goes to zero rather than to a wrong-but-plausible
	// number.
	byFence := map[string][]entur.Vehicle{}
	for i, p := range pts {
		byFence[key(p)] = []entur.Vehicle{{
			ID: fmt.Sprintf("v%d", i), OperatorKey: "ryde", Operator: "ryde",
			At:         geo.Point{Lat: p.Lat + 10/111320.0, Lon: p.Lon},
			RangeM:     20000,
			FormFactor: entur.ScooterStanding,
		}}
	}
	f := &slowFetcher{delay: 20 * time.Millisecond, byFence: byFence}
	p := &Poller{Client: f, Store: st, NearestInterval: -1}

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	for _, fence := range fences {
		got, _, err := st.LatestCounts(ctx, fence.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got["ryde"] != 1 {
			t.Errorf("fence %s recorded %d ryde, want 1; group results were crossed",
				fence.ID, got["ryde"])
		}
	}
}

// One group failing is one set of fences having a bad moment. The other fences
// were fetched perfectly well and their watches must still be evaluated.
func TestOneFailingGroupDoesNotCostTheOthersTheirTick(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	pts := farApart(3)
	fences := fencesAt(t, st, pts)

	// The first fence's query fails; the other two return a scooter.
	byFence := map[string][]entur.Vehicle{}
	for i, p := range pts[1:] {
		byFence[key(p)] = []entur.Vehicle{{
			ID: fmt.Sprintf("v%d", i+1), OperatorKey: "ryde", Operator: "ryde",
			At:         geo.Point{Lat: p.Lat + 10/111320.0, Lon: p.Lon},
			RangeM:     20000,
			FormFactor: entur.ScooterStanding,
		}}
	}
	boom := errors.New("connection reset")
	f := &slowFetcher{
		byFence: byFence,
		failAt: func(q entur.Query) error {
			if key(q.At) == key(pts[0]) {
				return boom
			}
			return nil
		},
	}

	var mu sync.Mutex
	var sent []Notification
	p := &Poller{Client: f, Store: st, NearestInterval: -1,
		Sink: SinkFunc(func(ctx context.Context, n Notification) error {
			mu.Lock()
			defer mu.Unlock()
			sent = append(sent, n)
			return nil
		}),
	}

	// Arm an appearance watch on each of the healthy fences.
	for _, fence := range fences[1:] {
		w := &store.Watch{
			ID: "w-" + fence.ID, Device: "phone", Kind: store.KindAppearance,
			FenceID: fence.ID, OperatorKeys: []string{"ryde"},
			CreatedAt: t0, ExpiresAt: t0.Add(time.Hour),
		}
		if err := st.CreateWatch(ctx, w); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := p.Tick(ctx, t0)
	if err == nil {
		t.Error("want the failed group reported, got nil")
	} else if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to carry the upstream failure", err)
	}
	if len(rep.Fired) != 2 {
		t.Errorf("Fired = %v, want both healthy fences' watches to fire despite the third failing",
			rep.Fired)
	}
	p.Wait()
	mu.Lock()
	n := len(sent)
	mu.Unlock()
	if n != 2 {
		t.Errorf("delivered %d notifications, want 2", n)
	}
}

// The per-operator nearest probes are up to four more round trips on the same
// tick the watches depend on.
func TestNearestProbesRunConcurrently(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	fencesAt(t, st, farApart(1))

	f := &slowFetcher{
		delay:       50 * time.Millisecond,
		byFence:     map[string][]entur.Vehicle{},
		perOperator: map[string][]entur.Vehicle{},
	}
	p := &Poller{Client: f, Store: st}

	rep, err := p.Tick(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NearestQueries != len(entur.Operators()) {
		t.Fatalf("NearestQueries = %d, want one per operator (%d)",
			rep.NearestQueries, len(entur.Operators()))
	}
	// The main query runs alone, then every probe together.
	if got := f.peak.Load(); int(got) < len(entur.Operators()) {
		t.Errorf("peak concurrent queries = %d, want %d; the probes ran in sequence",
			got, len(entur.Operators()))
	}
}

// A probe failing is context lost, not a tick lost. This invariant predates the
// parallel rewrite and is easy to drop in one.
func TestAFailingNearestProbeStillLeavesTheTickHealthy(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	fencesAt(t, st, farApart(1))

	f := &slowFetcher{
		byFence:     map[string][]entur.Vehicle{},
		perOperator: map[string][]entur.Vehicle{},
		failAt: func(q entur.Query) error {
			if len(q.OperatorKeys) == 1 {
				return errors.New("probe failed")
			}
			return nil
		},
	}
	p := &Poller{Client: f, Store: st}

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Errorf("a failed nearest probe must not fail the tick: %v", err)
	}
}

// The nearest probes fan out the same way the group queries do, so they invite
// the same crossed-wire bug: each probe's answer must be recorded against the
// operator that was asked for, not whichever reply landed first.
func TestEachNearestProbeIsAttributedToItsOwnOperator(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	fencesAt(t, st, farApart(1))
	fenceAt := farApart(1)[0]

	// Give every operator a vehicle at a distinct, recognisable distance, so a
	// swapped attribution shows up as the wrong number rather than as nothing.
	ops := entur.Operators()
	perOperator := map[string][]entur.Vehicle{}
	wantM := map[string]int{}
	for i, o := range ops {
		metres := float64((i + 1) * 500)
		perOperator[o.Key] = []entur.Vehicle{{
			ID: "v-" + o.Key, OperatorKey: o.Key, Operator: o.Name,
			At:         geo.Point{Lat: fenceAt.Lat + metres/111320.0, Lon: fenceAt.Lon},
			RangeM:     20000,
			FormFactor: entur.ScooterStanding,
		}}
		wantM[o.Key] = int(metres)
	}

	f := &slowFetcher{
		// A staggered delay makes the replies land out of order on purpose.
		delay:       10 * time.Millisecond,
		byFence:     map[string][]entur.Vehicle{},
		perOperator: perOperator,
	}
	p := &Poller{Client: f, Store: st}

	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestNearest(ctx, "f0")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range ops {
		n, ok := got[o.Key]
		if !ok {
			t.Errorf("no nearest recorded for %s", o.Key)
			continue
		}
		if n.DistanceM == nil {
			t.Errorf("%s recorded no distance, want ~%d m", o.Key, wantM[o.Key])
			continue
		}
		// Allow a metre of rounding in the great-circle maths.
		if diff := *n.DistanceM - wantM[o.Key]; diff < -2 || diff > 2 {
			t.Errorf("%s nearest = %d m, want ~%d m; probe results were crossed",
				o.Key, *n.DistanceM, wantM[o.Key])
		}
		if n.VehicleID != "v-"+o.Key {
			t.Errorf("%s nearest vehicle = %q, want %q; probe results were crossed",
				o.Key, n.VehicleID, "v-"+o.Key)
		}
	}
}
