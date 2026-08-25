package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/poll"
	"github.com/deviationist/scootless/internal/store"
)

type captured struct {
	path    string
	headers http.Header
	body    string
}

func ntfyServer(t *testing.T, status int) (*Ntfy, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.headers, got.body = r.URL.Path, r.Header.Clone(), string(b)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return &Ntfy{Server: srv.URL, Topic: "some-long-random-topic"}, got
}

func appearance() poll.Notification {
	pct := 27.0
	return poll.Notification{
		WatchID: "w1", Kind: store.KindAppearance,
		FiredAt: sampleNotification().FiredAt,
		Fence:   store.Fence{ID: "home", Name: "home", RadiusM: 100},
		Count:   2,
		Vehicles: []entur.Vehicle{
			{ID: "v1", Operator: "Ryde", DistanceM: 61.4, BearingDeg: 270,
				RangeM: 36200, AppLinkIOS: "ryde://vehicle/v1"},
			{ID: "v2", Operator: "Bolt", DistanceM: 80.2, BearingDeg: 135,
				RangeM: 12600, FuelPct: &pct},
		},
	}
}

func TestNtfyPostsToTopicWithUsefulHeaders(t *testing.T) {
	n, got := ntfyServer(t, 200)
	if err := n.Publish(context.Background(), appearance()); err != nil {
		t.Fatal(err)
	}
	if got.path != "/some-long-random-topic" {
		t.Errorf("path = %q", got.path)
	}
	if title := got.headers.Get("Title"); title != "Ryde 61 m away" {
		t.Errorf("Title = %q", title)
	}
	// Tapping it should land on the vehicle in the operator's own app - the
	// alert is meant to be one tap from a ride.
	if click := got.headers.Get("Click"); click != "ryde://vehicle/v1" {
		t.Errorf("Click = %q, want the app deep link", click)
	}
	if !strings.Contains(got.body, "Ryde · 61 m W · 36.2 km range") {
		t.Errorf("body = %q", got.body)
	}
	if !strings.Contains(got.body, "27%") {
		t.Errorf("battery missing from body: %q", got.body)
	}
}

// "Go now" and "you are running low" deserve different amounts of noise.
func TestNtfyPrioritisesAppearanceAboveScarcity(t *testing.T) {
	n, got := ntfyServer(t, 200)
	if err := n.Publish(context.Background(), appearance()); err != nil {
		t.Fatal(err)
	}
	high, _ := strconv.Atoi(got.headers.Get("Priority"))

	scarce := appearance()
	scarce.Kind = store.KindScarcity
	if err := n.Publish(context.Background(), scarce); err != nil {
		t.Fatal(err)
	}
	low, _ := strconv.Atoi(got.headers.Get("Priority"))

	if !(high > low) {
		t.Errorf("appearance priority %d should exceed scarcity priority %d", high, low)
	}
	if got.headers.Get("Title") != "2 left near home" {
		t.Errorf("scarcity title = %q", got.headers.Get("Title"))
	}
}

func TestNtfySendsBearerTokenWhenConfigured(t *testing.T) {
	n, got := ntfyServer(t, 200)
	n.Token = "tk_secret"
	if err := n.Publish(context.Background(), appearance()); err != nil {
		t.Fatal(err)
	}
	if got.headers.Get("Authorization") != "Bearer tk_secret" {
		t.Errorf("Authorization = %q", got.headers.Get("Authorization"))
	}
}

func TestNtfyNoTokenSendsNoAuthorizationHeader(t *testing.T) {
	n, got := ntfyServer(t, 200)
	if err := n.Publish(context.Background(), appearance()); err != nil {
		t.Fatal(err)
	}
	if got.headers.Get("Authorization") != "" {
		t.Errorf("Authorization sent despite no token: %q", got.headers.Get("Authorization"))
	}
}

func TestNtfySurfacesServerErrors(t *testing.T) {
	n, _ := ntfyServer(t, 403)
	err := n.Publish(context.Background(), appearance())
	if err == nil {
		t.Fatal("want an error on a 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want it to name the status", err)
	}
}

func TestNtfyRequiresServerAndTopic(t *testing.T) {
	if err := (&Ntfy{Topic: "t"}).Publish(context.Background(), appearance()); err == nil {
		t.Error("want an error with no server")
	}
	if err := (&Ntfy{Server: "http://x"}).Publish(context.Background(), appearance()); err == nil {
		t.Error("want an error with no topic")
	}
}

// A scarcity alert can legitimately carry no vehicles at all.
func TestNtfyHandlesAnEmptyVehicleList(t *testing.T) {
	n, got := ntfyServer(t, 200)
	empty := appearance()
	empty.Vehicles = nil
	empty.Count = 0
	if err := n.Publish(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	if got.body == "" {
		t.Error("empty body")
	}
	if got.headers.Get("Click") != "" {
		t.Errorf("Click set with no vehicles: %q", got.headers.Get("Click"))
	}
}

// --- Multi -----------------------------------------------------------------

type recordingSink struct {
	calls int
	err   error
}

func (r *recordingSink) Publish(ctx context.Context, n poll.Notification) error {
	r.calls++
	return r.err
}

// If MQTT is down the phone should still buzz, and vice versa.
func TestMultiDeliversToEverySinkDespiteAFailure(t *testing.T) {
	bad := &recordingSink{err: errors.New("broker down")}
	good := &recordingSink{}
	m := Multi{bad, good}

	err := m.Publish(context.Background(), appearance())
	if err == nil {
		t.Error("want the failure reported")
	}
	if good.calls != 1 {
		t.Errorf("healthy sink called %d times, want 1", good.calls)
	}
}

func TestMultiIsQuietWhenAllSucceed(t *testing.T) {
	a, b := &recordingSink{}, &recordingSink{}
	if err := (Multi{a, b, nil}).Publish(context.Background(), appearance()); err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("calls = %d, %d", a.calls, b.calls)
	}
}

func TestMultiReportsEveryFailure(t *testing.T) {
	a := &recordingSink{err: errors.New("first")}
	b := &recordingSink{err: errors.New("second")}
	err := (Multi{a, b}).Publish(context.Background(), appearance())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}
