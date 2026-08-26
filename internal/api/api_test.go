package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/store"
)

var now = time.Unix(1787652000, 0).UTC()

type fakeFetcher struct {
	vehicles []entur.Vehicle
	err      error
	queries  []entur.Query
}

func (f *fakeFetcher) Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error) {
	f.queries = append(f.queries, q)
	if f.err != nil {
		return nil, f.err
	}
	return &entur.Result{Vehicles: f.vehicles, Returned: len(f.vehicles)}, nil
}

func newServer(t *testing.T) (*Server, *fakeFetcher, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeFetcher{}
	s := &Server{Store: st, Client: f, Now: func() time.Time { return now }}
	if err := st.SaveFence(context.Background(), store.Fence{
		ID: "home", Name: "home", At: geo.Point{Lat: 59.9139, Lon: 10.7522}, RadiusM: 150,
	}); err != nil {
		t.Fatal(err)
	}
	return s, f, st
}

func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), dst); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
}

func TestHealthz(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("code = %d", w.Code)
	}
}

func TestVehiclesRequiresCoordinates(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "GET", "/api/v1/vehicles", ""); w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestVehiclesReturnsTheAsk(t *testing.T) {
	s, f, _ := newServer(t)
	pct := 35.0
	f.vehicles = []entur.Vehicle{{
		ID: "v1", Operator: "Ryde", OperatorKey: "ryde",
		DistanceM: 61.4, BearingDeg: 270, RangeM: 36200, FuelPct: &pct,
	}}
	w := do(t, s, "GET", "/api/v1/vehicles?lat=59.9139&lon=10.7522&radius=200", "")
	if w.Code != 200 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Count    int `json:"count"`
		Vehicles []struct {
			DistanceM int     `json:"distance_m"`
			Bearing   string  `json:"bearing"`
			RangeKM   float64 `json:"range_km"`
		} `json:"vehicles"`
	}
	decodeBody(t, w, &got)
	if got.Count != 1 || got.Vehicles[0].DistanceM != 61 || got.Vehicles[0].Bearing != "W" {
		t.Errorf("got %+v", got)
	}
	if f.queries[0].RadiusM != 200 {
		t.Errorf("radius passed through as %d", f.queries[0].RadiusM)
	}
}

// An unknown operator returns an empty list upstream rather than an error, so
// rejecting it here is what stops a typo looking like "no scooters nearby".
func TestVehiclesRejectsUnknownOperator(t *testing.T) {
	s, f, _ := newServer(t)
	w := do(t, s, "GET", "/api/v1/vehicles?lat=59.9&lon=10.7&operators=tier", "")
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
	if len(f.queries) != 0 {
		t.Error("the upstream was queried despite an unknown operator")
	}
}

func TestVehiclesReportsUpstreamFailureAsBadGateway(t *testing.T) {
	s, f, _ := newServer(t)
	f.err = context.DeadlineExceeded
	w := do(t, s, "GET", "/api/v1/vehicles?lat=59.9&lon=10.7", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("code = %d, want 502", w.Code)
	}
}

func TestCreateWatchOnANamedFenceCapturesBaseline(t *testing.T) {
	s, f, st := newServer(t)
	// Two scooters are already there when the watch is armed.
	f.vehicles = []entur.Vehicle{
		{ID: "a", OperatorKey: "ryde"}, {ID: "b", OperatorKey: "ryde"},
	}
	w := do(t, s, "POST", "/api/v1/watches",
		`{"device":"phone","kind":"appearance","fence_id":"home","operators":["ryde"]}`)
	if w.Code != 201 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got watchJSON
	decodeBody(t, w, &got)
	if got.Baseline != 2 {
		t.Errorf("baseline_size = %d, want 2", got.Baseline)
	}
	if got.State != "armed" {
		t.Errorf("state = %q", got.State)
	}
	if !got.ExpiresAt.Equal(now.Add(DefaultTTL)) {
		t.Errorf("ExpiresAt = %v, want now+%v", got.ExpiresAt, DefaultTTL)
	}

	stored, err := st.Watch(context.Background(), got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Baseline) != 2 {
		t.Errorf("stored baseline = %v", stored.Baseline)
	}
}

// "Watch around where I am standing right now" - the app's main case.
func TestCreateWatchWithAdHocPositionMakesAFence(t *testing.T) {
	s, _, st := newServer(t)
	w := do(t, s, "POST", "/api/v1/watches",
		`{"device":"phone","lat":59.92,"lon":10.75,"radius_m":250}`)
	if w.Code != 201 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got watchJSON
	decodeBody(t, w, &got)

	f, err := st.Fence(context.Background(), got.FenceID)
	if err != nil {
		t.Fatalf("ad-hoc fence not stored: %v", err)
	}
	if f.RadiusM != 250 {
		t.Errorf("radius = %d, want 250", f.RadiusM)
	}
	fences, _ := st.Fences(context.Background())
	if len(fences) != 2 {
		t.Errorf("fences = %d, want the named one plus the ad-hoc one", len(fences))
	}
}

func TestCreateWatchNeedsAPlace(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/api/v1/watches", `{"device":"phone"}`)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestCreateWatchRejectsUnknownFenceAndKind(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "POST", "/api/v1/watches", `{"fence_id":"nope"}`); w.Code != 400 {
		t.Errorf("unknown fence: code = %d, want 400", w.Code)
	}
	if w := do(t, s, "POST", "/api/v1/watches", `{"fence_id":"home","kind":"vibes"}`); w.Code != 400 {
		t.Errorf("unknown kind: code = %d, want 400", w.Code)
	}
}

// No watch polls forever, however long the caller asks for.
func TestWatchTTLIsCapped(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/api/v1/watches",
		`{"fence_id":"home","ttl_seconds":86400}`)
	var got watchJSON
	decodeBody(t, w, &got)
	if !got.ExpiresAt.Equal(now.Add(MaxTTL)) {
		t.Errorf("ExpiresAt = %v, want it capped at now+%v", got.ExpiresAt, MaxTTL)
	}
}

func TestListGetAndCancelWatch(t *testing.T) {
	s, _, st := newServer(t)
	w := do(t, s, "POST", "/api/v1/watches", `{"fence_id":"home","device":"phone"}`)
	var created watchJSON
	decodeBody(t, w, &created)

	var list struct {
		Watches []watchJSON `json:"watches"`
	}
	decodeBody(t, do(t, s, "GET", "/api/v1/watches", ""), &list)
	if len(list.Watches) != 1 {
		t.Fatalf("watches = %d, want 1", len(list.Watches))
	}

	decodeBody(t, do(t, s, "GET", "/api/v1/watches?device=other", ""), &list)
	if len(list.Watches) != 0 {
		t.Errorf("device filter ignored: %+v", list.Watches)
	}

	if w := do(t, s, "GET", "/api/v1/watches/"+created.ID, ""); w.Code != 200 {
		t.Errorf("get: code = %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/watches/nope", ""); w.Code != 404 {
		t.Errorf("get missing: code = %d, want 404", w.Code)
	}

	if w := do(t, s, "DELETE", "/api/v1/watches/"+created.ID, ""); w.Code != 204 {
		t.Errorf("cancel: code = %d, want 204", w.Code)
	}
	stored, _ := st.Watch(context.Background(), created.ID)
	if stored.State != store.StateCancelled {
		t.Errorf("state = %q, want cancelled", stored.State)
	}
	if w := do(t, s, "DELETE", "/api/v1/watches/nope", ""); w.Code != 404 {
		t.Errorf("cancel missing: code = %d, want 404", w.Code)
	}
}

func TestHistoryAndArrivals(t *testing.T) {
	s, _, st := newServer(t)
	ctx := context.Background()
	st.RecordSample(ctx, "home", now.Add(-time.Hour), "ryde", 2)
	st.ObservePresence(ctx, "home", now.Add(-time.Hour),
		[]store.Sighting{{VehicleID: "a", Operator: "ryde"}}, store.DefaultStale)

	var hist struct {
		Samples []map[string]any `json:"samples"`
	}
	w := do(t, s, "GET", "/api/v1/history?fence=home", "")
	if w.Code != 200 {
		t.Fatalf("history: code = %d: %s", w.Code, w.Body)
	}
	decodeBody(t, w, &hist)
	if len(hist.Samples) != 1 {
		t.Errorf("samples = %d, want 1", len(hist.Samples))
	}

	var arr struct {
		Count int `json:"count"`
	}
	w = do(t, s, "GET", "/api/v1/arrivals?fence=home", "")
	decodeBody(t, w, &arr)
	if arr.Count != 1 {
		t.Errorf("arrivals = %d, want 1", arr.Count)
	}
}

func TestHistoryValidatesTheWindow(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "GET", "/api/v1/history", ""); w.Code != 400 {
		t.Errorf("missing fence: code = %d, want 400", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/history?fence=home&from=yesterday", ""); w.Code != 400 {
		t.Errorf("bad from: code = %d, want 400", w.Code)
	}
	if w := do(t, s, "GET",
		"/api/v1/history?fence=home&from=2026-08-25T10:00:00Z&to=2026-08-25T09:00:00Z", ""); w.Code != 400 {
		t.Errorf("reversed window: code = %d, want 400", w.Code)
	}
}

func TestCreateFence(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/api/v1/fences",
		`{"id":"work","name":"work","lat":59.92,"lon":10.76,"radius_m":200}`)
	if w.Code != 201 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var list struct {
		Fences []fenceJSON `json:"fences"`
	}
	decodeBody(t, do(t, s, "GET", "/api/v1/fences", ""), &list)
	if len(list.Fences) != 2 {
		t.Errorf("fences = %d, want 2", len(list.Fences))
	}
}

func TestCreateFenceRejectsBadCoordinates(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/api/v1/fences", `{"lat":123,"lon":10}`)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestUnknownJSONFieldIsRejected(t *testing.T) {
	s, _, _ := newServer(t)
	w := do(t, s, "POST", "/api/v1/watches", `{"fence_id":"home","opperators":["ryde"]}`)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400 - a misspelled field must not be silently ignored", w.Code)
	}
}

func TestTokenGatesTheAPIButNotHealth(t *testing.T) {
	s, _, _ := newServer(t)
	s.Token = "sekret"

	if w := do(t, s, "GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz should stay open: code = %d", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/fences", ""); w.Code != 401 {
		t.Errorf("no token: code = %d, want 401", w.Code)
	}

	r := httptest.NewRequest("GET", "/api/v1/fences", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("wrong token: code = %d, want 401", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/v1/fences", nil)
	r.Header.Set("Authorization", "Bearer sekret")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("right token: code = %d, want 200", w.Code)
	}
}

// --- status ----------------------------------------------------------------

func seedStatus(t *testing.T, st *store.Store, counts map[string]int, near map[string]int, absent []string) {
	t.Helper()
	ctx := context.Background()
	for op, n := range counts {
		if err := st.RecordSample(ctx, "home", now, op, n); err != nil {
			t.Fatal(err)
		}
	}
	for op, d := range near {
		d := d
		if err := st.RecordNearest(ctx, "home", now,
			store.Nearest{Operator: op, At: now, DistanceM: &d, VehicleID: "v-" + op}); err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range absent {
		if err := st.RecordNearest(ctx, "home", now,
			store.Nearest{Operator: op, At: now}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStatusSummaryReadsLikeASentence(t *testing.T) {
	s, _, st := newServer(t)
	seedStatus(t,
		st,
		map[string]int{"bolt": 2, "voi": 0, "ryde": 0},
		map[string]int{"bolt": 40, "voi": 173, "ryde": 481},
		nil)

	w := do(t, s, "GET", "/api/v1/status?fence=home&operators=ryde,voi,bolt", "")
	if w.Code != 200 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Summary string         `json:"summary"`
		Counts  map[string]int `json:"counts"`
	}
	decodeBody(t, w, &got)

	want := "2 Bolt available, 173 m to nearest Voi, 481 m to nearest Ryde"
	if got.Summary != want {
		t.Errorf("summary =\n  %q\nwant\n  %q", got.Summary, want)
	}
	if got.Counts["ryde"] != 0 || got.Counts["bolt"] != 2 {
		t.Errorf("counts = %v", got.Counts)
	}
}

// "No Ryde within reach" is a different decision from "one 481 m away", so it
// must not render as a distance of zero or vanish.
func TestStatusDistinguishesAbsentFromFarAway(t *testing.T) {
	s, _, st := newServer(t)
	seedStatus(t, st,
		map[string]int{"ryde": 0, "voi": 0},
		map[string]int{"voi": 900},
		[]string{"ryde"})

	var got struct {
		Summary string `json:"summary"`
		Nearest map[string]struct {
			DistanceM *int `json:"distance_m"`
		} `json:"nearest"`
	}
	decodeBody(t, do(t, s, "GET", "/api/v1/status?fence=home&operators=ryde,voi", ""), &got)

	if got.Nearest["ryde"].DistanceM != nil {
		t.Errorf("ryde distance = %v, want null", *got.Nearest["ryde"].DistanceM)
	}
	want := "900 m to nearest Voi, no Ryde nearby"
	if got.Summary != want {
		t.Errorf("summary = %q, want %q", got.Summary, want)
	}
}

// An available scooter beats any distance, so presence sorts ahead.
func TestStatusPutsAvailableOperatorsFirst(t *testing.T) {
	s, _, st := newServer(t)
	seedStatus(t, st,
		map[string]int{"ryde": 0, "bolt": 1},
		map[string]int{"ryde": 20, "bolt": 300},
		nil)
	var got struct {
		Summary string `json:"summary"`
	}
	decodeBody(t, do(t, s, "GET", "/api/v1/status?fence=home&operators=ryde,bolt", ""), &got)
	want := "1 Bolt available, 20 m to nearest Ryde"
	if got.Summary != want {
		t.Errorf("summary = %q, want %q", got.Summary, want)
	}
}

func TestStatusBeforeAnyDataIsCollected(t *testing.T) {
	s, _, _ := newServer(t)
	var got struct {
		Summary string `json:"summary"`
	}
	w := do(t, s, "GET", "/api/v1/status?fence=home", "")
	if w.Code != 200 {
		t.Fatalf("code = %d: %s", w.Code, w.Body)
	}
	decodeBody(t, w, &got)
	if got.Summary != "nothing known yet" {
		t.Errorf("summary = %q", got.Summary)
	}
}

func TestStatusValidatesFence(t *testing.T) {
	s, _, _ := newServer(t)
	if w := do(t, s, "GET", "/api/v1/status", ""); w.Code != 400 {
		t.Errorf("missing fence: code = %d, want 400", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/status?fence=nope", ""); w.Code != 404 {
		t.Errorf("unknown fence: code = %d, want 404", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/status?fence=home&operators=tier", ""); w.Code != 400 {
		t.Errorf("bad operator: code = %d, want 400", w.Code)
	}
}

// A browser EventSource cannot set headers, so the token is accepted as a
// query parameter as a fallback.
func TestTokenAcceptedAsQueryParam(t *testing.T) {
	s, _, _ := newServer(t)
	s.Token = "sekret"

	if w := do(t, s, "GET", "/api/v1/fences?token=sekret", ""); w.Code != 200 {
		t.Errorf("?token=valid: code = %d, want 200", w.Code)
	}
	if w := do(t, s, "GET", "/api/v1/fences?token=wrong", ""); w.Code != 401 {
		t.Errorf("?token=wrong: code = %d, want 401", w.Code)
	}
	// header still works and takes precedence
	r := httptest.NewRequest("GET", "/api/v1/fences?token=wrong", nil)
	r.Header.Set("Authorization", "Bearer sekret")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Errorf("header should win over a wrong query token: code = %d", w.Code)
	}
}
