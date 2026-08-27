package poll

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/store"
)

// deliveryHarness arms one appearance watch that will fire on the first tick.
func deliveryHarness(t *testing.T, sink Sink) *Poller {
	t.Helper()
	st := newStore(t)
	if err := st.SaveFence(context.Background(), store.Fence{
		ID: "home", Name: "home", At: home, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWatch(context.Background(), &store.Watch{
		ID: "w1", Device: "phone", Kind: store.KindAppearance, FenceID: "home",
		OperatorKeys: []string{"ryde"},
		CreatedAt:    t0, ExpiresAt: t0.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return &Poller{
		Client: &fakeFetcher{vehicles: []entur.Vehicle{
			vehicle("new", "ryde", 50, 20000),
		}},
		Store: st, Sink: sink, NearestInterval: -1,
	}
}

// Delivery is a network round trip that happens after the watch has already
// fired and been recorded, so nothing left to decide depends on it. It used to
// run inline, which meant a second watch firing on the same tick queued behind
// the first one's broker acknowledgement.
func TestNotificationDeliveryDoesNotBlockTheTick(t *testing.T) {
	release := make(chan struct{})
	var delivered sync.WaitGroup
	delivered.Add(1)

	p := deliveryHarness(t, SinkFunc(func(ctx context.Context, n Notification) error {
		defer delivered.Done()
		<-release
		return nil
	}))

	start := time.Now()
	rep, err := p.Tick(context.Background(), t0)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if len(rep.Fired) != 1 {
		t.Fatalf("Fired = %v, want [w1]", rep.Fired)
	}
	if elapsed > time.Second {
		t.Errorf("tick took %v while a sink was blocked; delivery is on the critical path", elapsed)
	}

	close(release)
	delivered.Wait()
	p.Wait()
}

// A tick's context is cancelled when the tick ends. A notification that
// inherited it would be cancelled at exactly the moment it was handed over -
// the one failure this program exists to avoid.
func TestDeliverySurvivesTheTicksContextEnding(t *testing.T) {
	type result struct {
		err error
	}
	got := make(chan result, 1)
	started := make(chan struct{})

	p := deliveryHarness(t, SinkFunc(func(ctx context.Context, n Notification) error {
		close(started)
		// Give the caller time to cancel the tick's context underneath us.
		time.Sleep(50 * time.Millisecond)
		got <- result{err: ctx.Err()}
		return nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := p.Tick(ctx, t0); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel() // the tick is over; its context goes away

	p.Wait()
	select {
	case r := <-got:
		if r.err != nil {
			t.Errorf("delivery context was cancelled with the tick: %v", r.err)
		}
	default:
		t.Fatal("delivery never ran")
	}
}

// Wait is what makes the -once mode and the tests deterministic: without it a
// process can exit mid-POST.
func TestWaitBlocksUntilDeliveryFinishes(t *testing.T) {
	var done bool
	var mu sync.Mutex

	p := deliveryHarness(t, SinkFunc(func(ctx context.Context, n Notification) error {
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		done = true
		mu.Unlock()
		return nil
	}))

	if _, err := p.Tick(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	p.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !done {
		t.Error("Wait returned before the delivery finished")
	}
}

// Wait on a poller with no sink, and on one that never fired, must not hang.
func TestWaitIsSafeWithNothingInFlight(t *testing.T) {
	p := deliveryHarness(t, nil)
	if _, err := p.Tick(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait hung with no sink configured")
	}
}

// The delivery timeout has to be long enough that a slow phone still gets its
// notification, and finite so a wedged sink cannot leak goroutines forever.
func TestDeliveryTimeoutIsBoundedAndGenerous(t *testing.T) {
	if DeliveryTimeout <= 0 {
		t.Fatal("DeliveryTimeout must be finite; a wedged sink would leak goroutines")
	}
	if DeliveryTimeout < 10*time.Second {
		t.Errorf("DeliveryTimeout = %v; too short to survive a slow broker", DeliveryTimeout)
	}
}
