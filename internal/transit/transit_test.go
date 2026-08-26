package transit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/geo"
)

var oslo = geo.Point{Lat: 59.9139, Lon: 10.7522}

func fakeServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ET-Client-Name") == "" {
			t.Error("missing ET-Client-Name header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New("scootless-test")
	c.Endpoint = srv.URL
	c.Now = func() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }
	return c
}

func edge(name string, dist int, calls string) string {
	return `{"node":{"distance":` + itoa(dist) + `,"place":{"name":"` + name +
		`","id":"NSR:StopPlace:1","estimatedCalls":[` + calls + `]}}}`
}

func call(mode, line, dest, at string, realtime bool) string {
	rt := "false"
	if realtime {
		rt = "true"
	}
	return `{"expectedDepartureTime":"` + at + `","realtime":` + rt +
		`,"destinationDisplay":{"frontText":"` + dest +
		`"},"serviceJourney":{"line":{"publicCode":"` + line +
		`","transportMode":"` + mode + `"}}}`
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func wrap(edges string) string {
	return `{"data":{"nearest":{"edges":[` + edges + `]}}}`
}

func TestDeparturesParsedAndSortedByTime(t *testing.T) {
	c := fakeServer(t, wrap(
		edge("Torshov", 300,
			call("bus", "30", "Nydalen", "2026-08-26T12:10:00Z", true)+","+
				call("tram", "12", "Kjelsås", "2026-08-26T12:04:00Z", true))+","+
			edge("Vogts gate", 380,
				call("bus", "28", "Økern", "2026-08-26T12:02:00Z", false)),
	))
	deps, err := c.Departures(context.Background(), Query{At: oslo})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 3 {
		t.Fatalf("got %d departures, want 3", len(deps))
	}
	// soonest first: 12:02, 12:04, 12:10
	if deps[0].Line != "28" || deps[1].Line != "12" || deps[2].Line != "30" {
		t.Errorf("order = %s,%s,%s; want 28,12,30", deps[0].Line, deps[1].Line, deps[2].Line)
	}
	if deps[0].InMinutes != 2 {
		t.Errorf("InMinutes = %d, want 2", deps[0].InMinutes)
	}
	if deps[1].Stop != "Torshov" || deps[1].DistanceM != 300 {
		t.Errorf("stop info = %q %d", deps[1].Stop, deps[1].DistanceM)
	}
}

func TestDeparturesModeFilter(t *testing.T) {
	c := fakeServer(t, wrap(
		edge("X", 100,
			call("bus", "30", "A", "2026-08-26T12:05:00Z", true)+","+
				call("tram", "12", "B", "2026-08-26T12:06:00Z", true)),
	))
	deps, err := c.Departures(context.Background(), Query{At: oslo, Modes: []string{"tram"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Mode != "tram" {
		t.Errorf("mode filter failed: %+v", deps)
	}
}

func TestDeparturesLineFilter(t *testing.T) {
	c := fakeServer(t, wrap(
		edge("X", 100,
			call("bus", "21", "A", "2026-08-26T12:05:00Z", true)+","+
				call("bus", "30", "B", "2026-08-26T12:06:00Z", true)),
	))
	deps, err := c.Departures(context.Background(), Query{At: oslo, Lines: []string{"21"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Line != "21" {
		t.Errorf("line filter failed: %+v", deps)
	}
}

func TestDeparturesSurfacesErrors(t *testing.T) {
	c := fakeServer(t, `{"errors":[{"message":"bad query"}]}`)
	if _, err := c.Departures(context.Background(), Query{At: oslo}); err == nil {
		t.Error("want an error from the errors array")
	}
}

func TestLiveDepartures(t *testing.T) {
	if os.Getenv("SCOOTLESS_LIVE") != "1" {
		t.Skip("set SCOOTLESS_LIVE=1 to hit the real API")
	}
	c := New("scootless-test")
	deps, err := c.Departures(context.Background(), Query{At: oslo, RadiusM: 700})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("got %d departures near Oslo centre", len(deps))
	for i, d := range deps {
		if i >= 3 {
			break
		}
		t.Logf("  %s %s → %s in %d min (%s, %d m)", d.Mode, d.Line, d.Dest, d.InMinutes, d.Stop, d.DistanceM)
	}
}
