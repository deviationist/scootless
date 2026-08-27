package poll

import (
	"context"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/store"
)

// The poll interval is the dominant term in notification latency that is
// actually ours to set: a fixed interval adds a uniform 0-interval delay,
// half of it on average, on top of whatever the feed itself cost.
//
// Measured by sampling every operator at 2 s for 200 s, the gap between
// consecutive feed changes was 4, 26, 28, 4, 32, 22, 2, 2, 28, 6 seconds - a
// mean of about 15. An interval above that is undersampling a feed that is
// already moving.
func TestDefaultIntervalSamplesFasterThanTheFeedChanges(t *testing.T) {
	const measuredMeanGap = 15 * time.Second
	if DefaultInterval > measuredMeanGap {
		t.Errorf("DefaultInterval = %v, but the feed changes about every %v; "+
			"polling slower than the data moves adds latency for nothing",
			DefaultInterval, measuredMeanGap)
	}
	if DefaultInterval < MinInterval {
		t.Errorf("DefaultInterval = %v is below the floor %v", DefaultInterval, MinInterval)
	}
}

// The floor exists because a free public dataset should not be asked several
// times per change it makes.
func TestMinIntervalIsAFloorWorthHaving(t *testing.T) {
	if MinInterval <= 0 {
		t.Fatal("MinInterval must be positive")
	}
	if MinInterval > DefaultInterval {
		t.Errorf("MinInterval = %v exceeds DefaultInterval = %v", MinInterval, DefaultInterval)
	}
}

// A tick must not be able to outlive the interval that schedules it, or a slow
// request straddles the next tick.
func TestUpstreamTimeoutFitsInsideAnInterval(t *testing.T) {
	if entur.DefaultTimeout <= DefaultInterval {
		return // comfortably inside
	}
	// It may exceed one interval, but not so far that requests pile up.
	if entur.DefaultTimeout > 2*DefaultInterval {
		t.Errorf("upstream timeout %v is more than two poll intervals (%v); "+
			"slow requests will overlap ticks", entur.DefaultTimeout, DefaultInterval)
	}
}

// Run must poll immediately rather than waiting out the first interval: a
// daemon that knows nothing for its first interval is one that misses the
// scooter it was started for.
func TestRunTicksImmediately(t *testing.T) {
	st := newStore(t)
	if err := st.SaveFence(context.Background(), store.Fence{
		ID: "home", Name: "home", At: home, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	f := &slowFetcher{byFence: map[string][]entur.Vehicle{}, perOperator: map[string][]entur.Vehicle{}}
	p := &Poller{Client: f, Store: st, NearestInterval: -1,
		// An interval far longer than the test, so anything observed must be
		// the immediate first tick rather than a scheduled one.
		Interval: time.Hour,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for f.calls.Load() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not poll before the first interval elapsed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	p.Wait()
}
