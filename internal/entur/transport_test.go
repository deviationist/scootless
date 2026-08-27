package entur

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/geo"
)

func emptyVehiclesServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"vehicles": []any{}},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// A tick now issues several queries at once against a single host. The stdlib
// default keeps two idle connections per host, so most of them would open a
// fresh connection and pay a TLS handshake - measured at ~35 ms against Entur,
// on a request whose total is ~230 ms.
func TestSharedTransportPoolsEnoughConnectionsForOneTick(t *testing.T) {
	tr, ok := sharedTransport().(*http.Transport)
	if !ok {
		t.Fatal("sharedTransport is not an *http.Transport")
	}
	// One coalesced query plus a probe per operator, with room to spare.
	want := len(Operators()) + 1
	if tr.MaxIdleConnsPerHost < want {
		t.Errorf("MaxIdleConnsPerHost = %d, want at least %d so a whole tick's "+
			"fan-out is served from the pool", tr.MaxIdleConnsPerHost, want)
	}
	if tr.MaxIdleConns < want {
		t.Errorf("MaxIdleConns = %d, want at least %d", tr.MaxIdleConns, want)
	}
	if tr.IdleConnTimeout < time.Minute {
		t.Errorf("IdleConnTimeout = %v; too short to survive between ticks", tr.IdleConnTimeout)
	}
}

// The pool is shared, so two clients built by New must not each get their own.
func TestSharedTransportIsShared(t *testing.T) {
	if sharedTransport() != sharedTransport() {
		t.Error("sharedTransport returned two different transports")
	}
	a, b := New("a"), New("b")
	if a.HTTP.Transport != b.HTTP.Transport {
		t.Error("two clients got separate transports; the connection pool is not shared")
	}
}

// Sequential queries must reuse the connection rather than dial every time.
func TestSequentialQueriesReuseTheConnection(t *testing.T) {
	srv := emptyVehiclesServer(t)
	c := &Client{Endpoint: srv.URL, ClientName: "test",
		HTTP: &http.Client{Timeout: DefaultTimeout, Transport: sharedTransport()}}

	var dialled atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) { dialled.Add(1) },
	})
	for range 5 {
		if _, err := c.Vehicles(ctx, Query{At: geo.Point{Lat: 59.9, Lon: 10.7}, RadiusM: 500}); err != nil {
			t.Fatal(err)
		}
	}
	if n := dialled.Load(); n > 1 {
		t.Errorf("dialled %d times for 5 sequential queries; connections are not being reused", n)
	}
}

// Concurrent queries are the point of the pool: they must not be capped to the
// stdlib's two idle connections per host.
func TestConcurrentQueriesAreNotSerialisedByTheTransport(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := inFlight.Add(1)
		for {
			p := peak.Load()
			if v <= p || peak.CompareAndSwap(p, v) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(40 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"vehicles": []any{}},
		})
	}))
	defer srv.Close()

	c := &Client{Endpoint: srv.URL, ClientName: "test",
		HTTP: &http.Client{Timeout: DefaultTimeout, Transport: sharedTransport()}}

	const n = 5
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Vehicles(context.Background(), Query{
				At: geo.Point{Lat: 59.9, Lon: 10.7}, RadiusM: 500,
			})
		}()
	}
	wg.Wait()

	if got := peak.Load(); int(got) < n {
		t.Errorf("peak concurrent requests = %d, want %d; the transport serialised them", got, n)
	}
}

// A request that outlives its own tick holds a slot while the next one starts,
// and whatever it returns describes a feed state already superseded.
func TestDefaultTimeoutIsBoundedAndSane(t *testing.T) {
	if DefaultTimeout <= 0 {
		t.Fatal("DefaultTimeout must be finite")
	}
	if DefaultTimeout > 20*time.Second {
		t.Errorf("DefaultTimeout = %v; a request should not outlive several ticks", DefaultTimeout)
	}
	if DefaultTimeout < 3*time.Second {
		t.Errorf("DefaultTimeout = %v; too tight for a slow-but-working upstream", DefaultTimeout)
	}
}

// New must produce a client that is actually usable and carries the header
// Entur asks for - the pooling change rewrote this constructor.
func TestNewBuildsAWorkingClient(t *testing.T) {
	var gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get("ET-Client-Name")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"vehicles": []any{}},
		})
	}))
	defer srv.Close()

	c := New("scootless-test")
	c.Endpoint = srv.URL
	if c.HTTP.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", c.HTTP.Timeout, DefaultTimeout)
	}
	if _, err := c.Vehicles(context.Background(), Query{
		At: geo.Point{Lat: 59.9, Lon: 10.7}, RadiusM: 500,
	}); err != nil {
		t.Fatal(err)
	}
	if gotName != "scootless-test" {
		t.Errorf("ET-Client-Name = %q, want scootless-test", gotName)
	}
}
