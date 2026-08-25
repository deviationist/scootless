// Package poll drives everything: it is the only part of scootless that talks
// upstream. On each tick it refreshes every fence, records history, and
// evaluates the armed watches.
package poll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/store"
)

// DefaultInterval is how often to poll.
//
// Chosen from measurement, not from the operators' stated ttl. Sampling the
// three Oslo feeds showed Ryde advancing every 27-33 s (mean 30, jittery
// rather than the exact 30 s steps a coarser earlier sample suggested), Voi
// irregular at 31-58 s, and Bolt only every ~5 minutes. There is no shared
// clock to lock onto, so a fixed interval a little under the fastest operator's
// cadence is the honest choice: it catches Ryde's ticks without aliasing, and
// for the slower operators the upstream cadence dominates the latency anyway.
const DefaultInterval = 20 * time.Second

// MaxQueryRadiusM caps how wide a coalesced query may grow. Beyond this, one
// query per fence costs less than over-fetching a whole city.
const MaxQueryRadiusM = 2000

// DefaultReachM is how far to look when answering "and if there is none here,
// how far away is the nearest one".
const DefaultReachM = 5000

// nearestProbeCount is how many vehicles the nearest lookup asks for. More
// than one, so that a disabled vehicle or a bicycle at the front of the queue
// cannot be mistaken for there being nothing at all.
const nearestProbeCount = 10

// DefaultNearestInterval is how often that question is re-asked.
//
// It is deliberately slower than the poll interval. The in-fence count drives
// the watches and must be fresh; "the nearest Ryde is 481 m away" is context
// for a human deciding whether to walk, and does not change meaningfully in
// twenty seconds.
const DefaultNearestInterval = 2 * time.Minute

// Fetcher is the upstream client, narrowed to what the poller needs so tests
// can substitute a fake.
type Fetcher interface {
	Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error)
}

// Notification is a watch reporting that its condition is met.
type Notification struct {
	Watch    *store.Watch    `json:"-"`
	WatchID  string          `json:"watch_id"`
	Kind     store.Kind      `json:"kind"`
	FiredAt  time.Time       `json:"fired_at"`
	Fence    store.Fence     `json:"-"`
	Count    int             `json:"count"`
	Vehicles []entur.Vehicle `json:"vehicles"`
}

// Sink delivers notifications. The first implementation publishes to MQTT; a
// sink is an interface so a second one is a new file rather than a change.
type Sink interface {
	Publish(ctx context.Context, n Notification) error
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(ctx context.Context, n Notification) error

// Publish calls f.
func (f SinkFunc) Publish(ctx context.Context, n Notification) error { return f(ctx, n) }

// Poller ties the upstream client, the store and the sink together.
type Poller struct {
	Client   Fetcher
	Store    *store.Store
	Sink     Sink
	Interval time.Duration
	Stale    time.Duration
	Log      *slog.Logger

	// ReachM is how far out to look for the nearest vehicle of an operator
	// that has nothing inside the fence. Zero means DefaultReachM.
	ReachM int

	// NearestInterval is how often to do that wider lookup. Zero means
	// DefaultNearestInterval; negative disables it.
	NearestInterval time.Duration

	// nearestAt remembers, per fence, when the wider lookup last ran.
	nearestAt map[string]time.Time

	// Now is overridable so tests can drive time.
	Now func() time.Time
}

// Report summarises one tick, for logging and for tests.
type Report struct {
	At        time.Time
	Queries   int
	Fences    int
	Arrivals  int
	Expired   int
	Fired     []string
	Truncated bool

	// NearestQueries counts the extra one-vehicle lookups this tick made.
	NearestQueries int
}

// Run polls until the context is cancelled.
func (p *Poller) Run(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	// Poll immediately rather than waiting out the first interval; a daemon
	// that knows nothing for its first 20 seconds is a daemon that misses the
	// scooter you started it for.
	if _, err := p.Tick(ctx, p.now()); err != nil {
		p.log().Error("first tick failed", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if _, err := p.Tick(ctx, p.now()); err != nil {
				// A failed tick is not fatal. The upstream API is a free public
				// service and will occasionally be unreachable; the next tick
				// is 20 seconds away.
				p.log().Error("tick failed", "err", err)
			}
		}
	}
}

// Tick runs one full pass: refresh every fence, record what is there, and
// evaluate the armed watches.
func (p *Poller) Tick(ctx context.Context, now time.Time) (Report, error) {
	rep := Report{At: now}

	expired, err := p.Store.ExpireWatches(ctx, now)
	if err != nil {
		return rep, fmt.Errorf("expiring watches: %w", err)
	}
	rep.Expired = expired

	fences, err := p.Store.Fences(ctx)
	if err != nil {
		return rep, fmt.Errorf("loading fences: %w", err)
	}
	if len(fences) == 0 {
		return rep, nil
	}
	rep.Fences = len(fences)

	watches, err := p.Store.ArmedWatches(ctx, now)
	if err != nil {
		return rep, fmt.Errorf("loading watches: %w", err)
	}

	groups := groupFences(fences)
	rep.Queries = len(groups)

	for _, g := range groups {
		// Query every operator and impose no range floor: one query then
		// serves every watch on these fences, whatever each one asks for.
		res, err := p.Client.Vehicles(ctx, entur.Query{
			At: g.At, RadiusM: g.RadiusM,
		})
		if err != nil {
			return rep, fmt.Errorf("querying upstream: %w", err)
		}
		if res.Truncated {
			rep.Truncated = true
		}

		for _, f := range g.Fences {
			inside := within(res.Vehicles, f)
			if err := p.recordFence(ctx, f, now, inside, &rep); err != nil {
				return rep, err
			}
			if err := p.recordNearest(ctx, f, now, inside, &rep); err != nil {
				return rep, err
			}
			for _, w := range watches {
				if w.FenceID != f.ID {
					continue
				}
				fired, err := p.evaluate(ctx, w, f, now, inside)
				if err != nil {
					return rep, err
				}
				if fired {
					rep.Fired = append(rep.Fired, w.ID)
				}
			}
		}
	}
	return rep, nil
}

// recordFence stores the per-operator counts and updates presence, which is
// what turns a sequence of polls into a history of arrivals.
func (p *Poller) recordFence(ctx context.Context, f store.Fence, now time.Time,
	inside []entur.Vehicle, rep *Report) error {

	counts := map[string]int{}
	sightings := make([]store.Sighting, 0, len(inside))
	for _, v := range inside {
		key := v.OperatorKey
		if key == "" {
			key = "unknown"
		}
		counts[key]++
		sightings = append(sightings, store.Sighting{VehicleID: v.ID, Operator: key})
	}
	// Record a zero for every operator we know about, not just the ones
	// present. "No Ryde here at 08:00" is the observation that matters, and a
	// missing row is indistinguishable from a missing poll.
	for _, o := range entur.Operators() {
		if _, ok := counts[o.Key]; !ok {
			counts[o.Key] = 0
		}
	}
	for op, n := range counts {
		if err := p.Store.RecordSample(ctx, f.ID, now, op, n); err != nil {
			return fmt.Errorf("recording sample: %w", err)
		}
	}

	arrivals, err := p.Store.ObservePresence(ctx, f.ID, now, sightings, p.Stale)
	if err != nil {
		return fmt.Errorf("observing presence: %w", err)
	}
	rep.Arrivals += len(arrivals)
	return nil
}

// evaluate decides whether one watch should fire, and fires it if so.
func (p *Poller) evaluate(ctx context.Context, w *store.Watch, f store.Fence,
	now time.Time, inside []entur.Vehicle) (bool, error) {

	matching := matches(inside, w)

	var hit []entur.Vehicle
	switch w.Kind {
	case store.KindAppearance:
		base := make(map[string]struct{}, len(w.Baseline))
		for _, id := range w.Baseline {
			base[id] = struct{}{}
		}
		for _, v := range matching {
			if _, known := base[v.ID]; !known {
				hit = append(hit, v)
			}
		}
		if len(hit) == 0 {
			return false, nil
		}
	case store.KindScarcity:
		if len(matching) > w.Threshold {
			return false, nil
		}
		hit = matching
	default:
		return false, fmt.Errorf("unknown watch kind %q", w.Kind)
	}

	// MarkFired disarms a non-repeating watch in the same statement, so a
	// concurrent tick cannot deliver the same notification twice. Losing that
	// race is normal, not an error.
	if err := p.Store.MarkFired(ctx, w.ID, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("marking fired: %w", err)
	}

	n := Notification{
		Watch: w, WatchID: w.ID, Kind: w.Kind, FiredAt: now,
		Fence: f, Count: len(matching), Vehicles: hit,
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return false, err
	}
	if err := p.Store.RecordEvent(ctx, w.ID, now, payload); err != nil {
		return false, fmt.Errorf("recording event: %w", err)
	}
	if p.Sink != nil {
		if err := p.Sink.Publish(ctx, n); err != nil {
			// The watch has already fired and been recorded. A failed delivery
			// must not un-fire it, so this is logged and not returned.
			p.log().Error("publishing notification", "watch", w.ID, "err", err)
		}
	}
	return true, nil
}

// matches narrows a fence's vehicles to those a given watch cares about.
func matches(vehicles []entur.Vehicle, w *store.Watch) []entur.Vehicle {
	want := map[string]struct{}{}
	for _, k := range w.OperatorKeys {
		want[k] = struct{}{}
	}
	out := make([]entur.Vehicle, 0, len(vehicles))
	for _, v := range vehicles {
		if len(want) > 0 {
			if _, ok := want[v.OperatorKey]; !ok {
				continue
			}
		}
		if v.RangeM < w.MinRangeM {
			continue
		}
		out = append(out, v)
	}
	return out
}

// within trims a coalesced query's results to one fence, and re-bases each
// vehicle's distance and bearing onto that fence.
//
// The re-basing is not cosmetic. The client fills DistanceM relative to the
// point that was queried, which for a coalesced query is the centre of a
// circle covering several fences, not any one of them. Everything downstream -
// which vehicle is nearest, and the "61 m west" in a notification - would
// otherwise be measured from a place nobody is standing.
func within(vehicles []entur.Vehicle, f store.Fence) []entur.Vehicle {
	out := make([]entur.Vehicle, 0, len(vehicles))
	for _, v := range vehicles {
		d := geo.DistanceM(f.At, v.At)
		if d > float64(f.RadiusM) {
			continue
		}
		v.DistanceM = d
		v.BearingDeg = geo.BearingDeg(f.At, v.At)
		out = append(out, v)
	}
	return out
}

// group is one upstream query and the fences it serves.
type group struct {
	At      geo.Point
	RadiusM int
	Fences  []store.Fence
}

// groupFences coalesces fences into as few upstream queries as possible.
//
// One query per fence per tick does not scale and is rude to a free dataset.
// If a single circle covering every fence stays under MaxQueryRadiusM, one
// query serves them all and the exact per-fence filtering happens locally,
// where it is free. Fences spread wider than that get their own queries,
// because over-fetching a whole city to serve two streets is worse.
func groupFences(fences []store.Fence) []group {
	if len(fences) == 0 {
		return nil
	}
	centres := make([]geo.Point, len(fences))
	radii := make([]float64, len(fences))
	for i, f := range fences {
		centres[i] = f.At
		radii[i] = float64(f.RadiusM)
	}
	mid, r := geo.BoundingRadiusM(centres, radii)
	if r <= MaxQueryRadiusM {
		return []group{{At: mid, RadiusM: int(r) + 1, Fences: fences}}
	}
	out := make([]group, 0, len(fences))
	for _, f := range fences {
		out = append(out, group{At: f.At, RadiusM: f.RadiusM, Fences: []store.Fence{f}})
	}
	return out
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// recordNearest answers "how far away is the nearest one of each operator",
// including operators with nothing inside the fence at all.
//
// For an operator that is present, the answer is already in hand and costs
// nothing. For one that is absent it takes a separate query - but only a
// small one, because the upstream API selects the nearest N rather than an
// arbitrary N (measured, not assumed), so a tiny count still returns the
// closest vehicles rather than a random sample.
func (p *Poller) recordNearest(ctx context.Context, f store.Fence, now time.Time,
	inside []entur.Vehicle, rep *Report) error {

	interval := p.NearestInterval
	if interval == 0 {
		interval = DefaultNearestInterval
	}
	if interval < 0 {
		return nil
	}

	// Nearest-in-fence is free: it falls out of the query already made, and
	// `inside` has already been re-based onto this fence.
	closest := map[string]entur.Vehicle{}
	for _, v := range inside {
		if v.OperatorKey == "" {
			continue
		}
		if cur, ok := closest[v.OperatorKey]; !ok || v.DistanceM < cur.DistanceM {
			closest[v.OperatorKey] = v
		}
	}
	for key, v := range closest {
		d := int(v.DistanceM + 0.5)
		if err := p.Store.RecordNearest(ctx, f.ID, now, store.Nearest{
			Operator: key, At: now, DistanceM: &d, VehicleID: v.ID,
		}); err != nil {
			return fmt.Errorf("recording nearest: %w", err)
		}
	}

	if p.nearestAt == nil {
		p.nearestAt = map[string]time.Time{}
	}
	last, seen := p.nearestAt[f.ID]
	if seen && now.Sub(last) < interval {
		return nil
	}
	p.nearestAt[f.ID] = now

	reach := p.ReachM
	if reach <= 0 {
		reach = DefaultReachM
	}
	for _, o := range entur.Operators() {
		if _, present := closest[o.Key]; present {
			continue
		}
		// Not Limit: 1. The upstream selects the nearest N before we filter
		// out the ones that cannot be rented, so asking for exactly one and
		// having it turn out to be disabled would read as "none within 5 km".
		// A small margin makes that impossible in practice.
		res, err := p.Client.Vehicles(ctx, entur.Query{
			At: f.At, RadiusM: reach, OperatorKeys: []string{o.Key},
			Limit: nearestProbeCount,
		})
		rep.NearestQueries++
		if err != nil {
			// One operator's nearest is context, not the product. Losing it
			// must not fail the tick that the watches depend on.
			p.log().Warn("nearest lookup failed", "operator", o.Key, "err", err)
			continue
		}
		n := store.Nearest{Operator: o.Key, At: now}
		if best, ok := nearestOf(res.Vehicles, f); ok {
			d := int(geo.DistanceM(f.At, best.At) + 0.5)
			n.DistanceM, n.VehicleID = &d, best.ID
		}
		// A nil distance is recorded deliberately: "no Ryde within 5 km" is a
		// different answer from "we did not look", and only one of them is
		// worth waking someone for.
		if err := p.Store.RecordNearest(ctx, f.ID, now, n); err != nil {
			return fmt.Errorf("recording nearest: %w", err)
		}
	}
	return nil
}

// nearestOf picks the vehicle closest to a fence, measured from the fence
// itself rather than from wherever the query happened to be centred.
func nearestOf(vehicles []entur.Vehicle, f store.Fence) (entur.Vehicle, bool) {
	var best entur.Vehicle
	bestD := math.Inf(1)
	found := false
	for _, v := range vehicles {
		if d := geo.DistanceM(f.At, v.At); d < bestD {
			best, bestD, found = v, d, true
		}
	}
	return best, found
}
