package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/deviationist/scootless/internal/poll"
)

// blockingSink records concurrency and can be told how long to take.
type blockingSink struct {
	delay time.Duration
	err   error

	inFlight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
}

func (s *blockingSink) Publish(ctx context.Context, n poll.Notification) error {
	v := s.inFlight.Add(1)
	for {
		peak := s.peak.Load()
		if v <= peak || s.peak.CompareAndSwap(peak, v) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	s.calls.Add(1)
	time.Sleep(s.delay)
	return s.err
}

// The sinks are independent destinations, each a round trip. In sequence the
// phone waited out the broker's acknowledgement before the ntfy POST even
// started, so the total was the sum rather than the slowest.
func TestMultiPublishesToEverySinkConcurrently(t *testing.T) {
	a := &blockingSink{delay: 80 * time.Millisecond}
	b := &blockingSink{delay: 80 * time.Millisecond}
	c := &blockingSink{delay: 80 * time.Millisecond}
	m := Multi{a, b, c}

	start := time.Now()
	if err := m.Publish(context.Background(), poll.Notification{WatchID: "w1"}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	for i, s := range []*blockingSink{a, b, c} {
		if s.calls.Load() != 1 {
			t.Errorf("sink %d called %d times, want 1", i, s.calls.Load())
		}
	}
	// Three 80 ms sinks in sequence take 240 ms.
	if elapsed > 200*time.Millisecond {
		t.Errorf("Publish took %v for 3 x 80ms sinks; they ran in sequence", elapsed)
	}
}

// Concurrency must not lose an error, and must not lose the *other* sinks when
// one fails. The index-keyed collection is the part a rewrite gets wrong.
func TestMultiReportsEveryFailureAndStillTriesEverySink(t *testing.T) {
	boom1 := errors.New("broker unreachable")
	boom2 := errors.New("ntfy 503")
	a := &blockingSink{err: boom1}
	ok := &blockingSink{}
	b := &blockingSink{err: boom2}

	err := Multi{a, ok, b}.Publish(context.Background(), poll.Notification{WatchID: "w1"})
	if err == nil {
		t.Fatal("want an error when two sinks fail")
	}
	if !errors.Is(err, boom1) || !errors.Is(err, boom2) {
		t.Errorf("err = %v, want it to carry both failures", err)
	}
	if ok.calls.Load() != 1 {
		t.Error("a healthy sink was skipped because another failed")
	}
}

// A single failure should surface as itself, not wrapped in a join.
func TestMultiReturnsALoneFailureUnwrapped(t *testing.T) {
	boom := errors.New("only one down")
	err := Multi{&blockingSink{}, &blockingSink{err: boom}}.
		Publish(context.Background(), poll.Notification{WatchID: "w1"})
	if err != boom {
		t.Errorf("err = %v, want exactly the one failure", err)
	}
}

func TestMultiSkipsNilSinks(t *testing.T) {
	ok := &blockingSink{}
	err := Multi{nil, ok, nil}.Publish(context.Background(), poll.Notification{WatchID: "w1"})
	if err != nil {
		t.Fatalf("nil sinks must be skipped, not failed: %v", err)
	}
	if ok.calls.Load() != 1 {
		t.Errorf("real sink called %d times, want 1", ok.calls.Load())
	}
}

func TestMultiWithNoSinksSucceeds(t *testing.T) {
	if err := (Multi{}).Publish(context.Background(), poll.Notification{}); err != nil {
		t.Errorf("empty Multi returned %v, want nil", err)
	}
}

// countingPublisher stands in for a broker and records concurrency.
type countingPublisher struct {
	delay time.Duration

	mu     sync.Mutex
	topics []string

	inFlight atomic.Int32
	peak     atomic.Int32
}

func (p *countingPublisher) Publish(topic string, qos byte, retained bool, payload any) mqtt.Token {
	v := p.inFlight.Add(1)
	for {
		peak := p.peak.Load()
		if v <= peak || p.peak.CompareAndSwap(peak, v) {
			break
		}
	}
	defer p.inFlight.Add(-1)
	time.Sleep(p.delay)
	p.mu.Lock()
	p.topics = append(p.topics, topic)
	p.mu.Unlock()
	return &fakeToken{}
}
func (p *countingPublisher) IsConnected() bool { return true }
func (p *countingPublisher) Disconnect(uint)   {}

// Two watches firing at once must not wait for each other's acknowledgement.
// paho documents its client as safe for concurrent use, so the lock that used
// to sit here bought nothing and cost exactly this.
func TestMQTTPublishesConcurrently(t *testing.T) {
	p := &countingPublisher{delay: 60 * time.Millisecond}
	m := New(p, "scootless", nil)

	start := time.Now()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Publish(context.Background(), poll.Notification{
				WatchID: string(rune('a' + i)),
			})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if got := p.peak.Load(); got < 4 {
		t.Errorf("peak concurrent publishes = %d, want 4; they serialised on a lock", got)
	}
	if elapsed > 180*time.Millisecond {
		t.Errorf("4 x 60ms publishes took %v; they serialised", elapsed)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.topics) != 4 {
		t.Fatalf("published %d messages, want 4", len(p.topics))
	}
	for _, tp := range p.topics {
		if !strings.HasPrefix(tp, "scootless/watch/") {
			t.Errorf("topic %q lost its prefix", tp)
		}
	}
}
