package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/poll"
)

// syncFetcher is safe for the concurrent access an SSE stream implies.
type syncFetcher struct {
	mu       sync.Mutex
	vehicles []entur.Vehicle
	calls    int
}

func (f *syncFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return &entur.Result{Vehicles: f.vehicles, Returned: len(f.vehicles)}, nil
}

func (f *syncFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The dashboard's staleness adds to the daemon's rather than hiding inside it.
// The stream used to default to 30 s on the grounds that the feed did not change
// faster, which measurement did not support: sampling every operator at 2 s, the
// gap between consecutive changes averaged about 15 s.
func TestStreamIntervalDefaultsToThePollInterval(t *testing.T) {
	def := int(poll.DefaultInterval / time.Second)
	if got := streamInterval(def); got != poll.DefaultInterval {
		t.Errorf("streamInterval(%d) = %v, want the poll interval %v",
			def, got, poll.DefaultInterval)
	}
	if poll.DefaultInterval > 15*time.Second {
		t.Errorf("the stream inherits DefaultInterval = %v, which is staler than "+
			"the ~15 s at which the feed was measured to change", poll.DefaultInterval)
	}
}

// The floor protects the upstream: each connection fetches independently, so a
// dashboard asking for one second would be a request per second per viewer.
func TestStreamIntervalClampsToTheSharedFloor(t *testing.T) {
	for _, secs := range []int{-5, 0, 1, 2} {
		if got := streamInterval(secs); got != poll.MinInterval {
			t.Errorf("streamInterval(%d) = %v, want it clamped to %v",
				secs, got, poll.MinInterval)
		}
	}
}

// Above the floor the request is honoured, so a dashboard can still ask for a
// slower, cheaper refresh.
func TestStreamIntervalHonoursAnythingAboveTheFloor(t *testing.T) {
	for _, secs := range []int{5, 20, 60, 300} {
		want := time.Duration(secs) * time.Second
		if want < poll.MinInterval {
			continue
		}
		if got := streamInterval(secs); got != want {
			t.Errorf("streamInterval(%d) = %v, want %v", secs, got, want)
		}
	}
}

// The floors must not drift apart: they are one politeness budget against one
// upstream.
func TestStreamFloorMatchesThePollerFloor(t *testing.T) {
	if poll.MinInterval <= 0 {
		t.Fatal("MinInterval must be positive")
	}
	if poll.DefaultInterval < poll.MinInterval {
		t.Errorf("DefaultInterval %v is below MinInterval %v",
			poll.DefaultInterval, poll.MinInterval)
	}
}

// The dashboard must render at once rather than waiting out the first tick -
// the same reasoning as the poller's immediate first tick.
func TestBoardStreamSendsAFrameImmediately(t *testing.T) {
	s, _, _ := newServer(t)
	f := &syncFetcher{vehicles: []entur.Vehicle{{
		ID: "v1", OperatorKey: "ryde", Operator: "Ryde",
		At: geo.Point{Lat: 59.9139, Lon: 10.7522}, RangeM: 20000,
		FormFactor: entur.ScooterStanding,
	}}}
	s.Client = f
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A long interval, so any frame we see must be the immediate one.
	req, _ := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/api/v1/board/stream?interval=600&lat=59.9139&lon=10.7522", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering must be no, or nginx delivers SSE in one lump")
	}

	sc := bufio.NewScanner(resp.Body)
	var frame string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			frame = sc.Text()
			break
		}
	}
	if frame == "" {
		t.Fatal("no frame arrived; the dashboard would render empty until the first tick")
	}
	if !strings.Contains(frame, "recommendation") {
		t.Errorf("first frame = %q, want a board", frame)
	}
	if f.count() != 1 {
		t.Errorf("fetched %d times for the immediate frame, want 1", f.count())
	}
}
