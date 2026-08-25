// Package api exposes scootless over HTTP: the one-shot ask, and the watches
// that make "I need a scooter" work.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/deviationist/scootless/internal/entur"
	"github.com/deviationist/scootless/internal/geo"
	"github.com/deviationist/scootless/internal/store"
)

// Defaults for watch lifetime. No watch polls forever.
const (
	DefaultTTL = 30 * time.Minute
	MaxTTL     = 6 * time.Hour
)

// Fetcher is the upstream client.
type Fetcher interface {
	Vehicles(ctx context.Context, q entur.Query) (*entur.Result, error)
}

// Server serves the API.
type Server struct {
	Store  *store.Store
	Client Fetcher
	Log    *slog.Logger

	// Token, when set, is required as a bearer token on every /api/ route.
	// A phone cannot log in interactively every morning without defeating
	// the point of the feature, so a long-lived per-device token is the
	// mechanism; see docs/BACKEND.md.
	Token string

	// Now is overridable so tests can drive time.
	Now func() time.Time
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/vehicles", s.vehicles)
	mux.HandleFunc("GET /api/v1/fences", s.listFences)
	mux.HandleFunc("POST /api/v1/fences", s.createFence)
	mux.HandleFunc("GET /api/v1/watches", s.listWatches)
	mux.HandleFunc("POST /api/v1/watches", s.createWatch)
	mux.HandleFunc("GET /api/v1/watches/{id}", s.getWatch)
	mux.HandleFunc("DELETE /api/v1/watches/{id}", s.cancelWatch)
	mux.HandleFunc("GET /api/v1/watches/{id}/events", s.watchEvents)
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/history", s.history)
	mux.HandleFunc("GET /api/v1/arrivals", s.arrivals)
	return s.authenticated(mux)
}

// authenticated gates /api/ behind the bearer token when one is configured.
// /healthz stays open so a supervisor can check liveness without a secret.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		// Constant-time, so the comparison cannot be used as an oracle.
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorised")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// vehicles answers the one-shot ask.
func (s *Server) vehicles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, lon, err := coords(q.Get("lat"), q.Get("lon"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	radius, err := intParam(q, "radius", 150)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	minRange, err := intParam(q, "min_range_m", 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ops := splitOperators(q.Get("operators"))
	if err := validOperators(ops); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.Client.Vehicles(r.Context(), entur.Query{
		At: geo.Point{Lat: lat, Lon: lon}, RadiusM: radius,
		OperatorKeys: ops, MinRangeM: minRange,
	})
	if err != nil {
		// The upstream is a free public service; when it is unreachable that
		// is worth distinguishing from a bad request.
		writeErr(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":     len(res.Vehicles),
		"truncated": res.Truncated,
		"radius_m":  radius,
		"vehicles":  vehiclesOf(res.Vehicles),
	})
}

func (s *Server) listFences(w http.ResponseWriter, r *http.Request) {
	fences, err := s.Store.Fences(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]fenceJSON, 0, len(fences))
	for _, f := range fences {
		out = append(out, fenceOf(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"fences": out})
}

type createFenceReq struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	RadiusM int     `json:"radius_m"`
}

func (s *Server) createFence(w http.ResponseWriter, r *http.Request) {
	var req createFenceReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.RadiusM <= 0 {
		req.RadiusM = 150
	}
	if err := checkCoords(req.Lat, req.Lon); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f := store.Fence{
		ID: orRandom(req.ID), Name: orDefault(req.Name, "fence"),
		At: geo.Point{Lat: req.Lat, Lon: req.Lon}, RadiusM: req.RadiusM,
	}
	if err := s.Store.SaveFence(r.Context(), f); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, fenceOf(f))
}

type createWatchReq struct {
	Device  string `json:"device"`
	Kind    string `json:"kind"`
	FenceID string `json:"fence_id"`

	// An ad-hoc fence, for "watch around where I am standing right now".
	Lat     *float64 `json:"lat"`
	Lon     *float64 `json:"lon"`
	RadiusM int      `json:"radius_m"`

	Operators  []string `json:"operators"`
	MinRangeM  int      `json:"min_range_m"`
	Threshold  int      `json:"threshold"`
	TTLSeconds int      `json:"ttl_seconds"`
	Repeat     bool     `json:"repeat"`
}

// createWatch arms a watch. This is the whole product in one call.
func (s *Server) createWatch(w http.ResponseWriter, r *http.Request) {
	var req createWatchReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()

	kind := store.Kind(orDefault(req.Kind, string(store.KindAppearance)))
	if kind != store.KindAppearance && kind != store.KindScarcity {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("unknown kind %q; want appearance or scarcity", req.Kind))
		return
	}
	if err := validOperators(req.Operators); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	fence, err := s.resolveFence(ctx, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}

	// The baseline is read live rather than from stored presence, because a
	// fence created a second ago has no history and a stale baseline would
	// either fire instantly or miss the first arrival.
	baseline, err := s.baseline(ctx, fence, req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}

	now := s.now()
	watch := &store.Watch{
		ID: randomID(), Device: orDefault(req.Device, "unknown"), Kind: kind,
		FenceID: fence.ID, OperatorKeys: req.Operators, MinRangeM: req.MinRangeM,
		Threshold: req.Threshold, Baseline: baseline, Repeat: req.Repeat,
		State: store.StateArmed, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := s.Store.CreateWatch(ctx, watch); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, watchOf(watch))
}

// resolveFence uses the named fence, or creates an ad-hoc one from a position.
func (s *Server) resolveFence(ctx context.Context, req createWatchReq) (store.Fence, error) {
	if req.FenceID != "" {
		f, err := s.Store.Fence(ctx, req.FenceID)
		if err != nil {
			return store.Fence{}, fmt.Errorf("unknown fence %q", req.FenceID)
		}
		return f, nil
	}
	if req.Lat == nil || req.Lon == nil {
		return store.Fence{}, errors.New("provide fence_id, or lat and lon")
	}
	if err := checkCoords(*req.Lat, *req.Lon); err != nil {
		return store.Fence{}, err
	}
	radius := req.RadiusM
	if radius <= 0 {
		radius = 150
	}
	f := store.Fence{
		ID: randomID(), Name: "here",
		At: geo.Point{Lat: *req.Lat, Lon: *req.Lon}, RadiusM: radius,
	}
	if err := s.Store.SaveFence(ctx, f); err != nil {
		return store.Fence{}, err
	}
	return f, nil
}

// baseline records what is already inside the fence, so an appearance watch
// fires on what is new rather than on what was always there.
func (s *Server) baseline(ctx context.Context, f store.Fence, req createWatchReq) ([]string, error) {
	res, err := s.Client.Vehicles(ctx, entur.Query{
		At: f.At, RadiusM: f.RadiusM,
		OperatorKeys: req.Operators, MinRangeM: req.MinRangeM,
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Vehicles))
	for _, v := range res.Vehicles {
		ids = append(ids, v.ID)
	}
	return ids, nil
}

func (s *Server) listWatches(w http.ResponseWriter, r *http.Request) {
	watches, err := s.Store.ArmedWatches(r.Context(), s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	device := r.URL.Query().Get("device")
	out := make([]watchJSON, 0, len(watches))
	for _, wa := range watches {
		if device != "" && wa.Device != device {
			continue
		}
		out = append(out, watchOf(wa))
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": out})
}

func (s *Server) getWatch(w http.ResponseWriter, r *http.Request) {
	wa, err := s.Store.Watch(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such watch")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, watchOf(wa))
}

func (s *Server) cancelWatch(w http.ResponseWriter, r *http.Request) {
	err := s.Store.SetState(r.Context(), r.PathValue("id"), store.StateCancelled)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such watch")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) watchEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"at": e.At, "payload": json.RawMessage(e.Payload),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	fence, from, to, err := s.window(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	points, err := s.Store.Samples(r.Context(), fence, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(points))
	for _, p := range points {
		out = append(out, map[string]any{
			"at": p.At, "operator": p.Operator, "count": p.Count,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fence_id": fence, "from": from, "to": to, "samples": out,
	})
}

func (s *Server) arrivals(w http.ResponseWriter, r *http.Request) {
	fence, from, to, err := s.window(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	arrivals, err := s.Store.Arrivals(r.Context(), fence, from, to)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(arrivals))
	for _, a := range arrivals {
		out = append(out, map[string]any{
			"at": a.At, "vehicle_id": a.VehicleID, "operator": a.Operator,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"fence_id": fence, "from": from, "to": to,
		"count": len(out), "arrivals": out,
	})
}

// window reads the fence and time range shared by the history endpoints.
func (s *Server) window(r *http.Request) (string, time.Time, time.Time, error) {
	q := r.URL.Query()
	fence := q.Get("fence")
	if fence == "" {
		return "", time.Time{}, time.Time{}, errors.New("fence is required")
	}
	to := s.now()
	from := to.Add(-24 * time.Hour)
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return "", time.Time{}, time.Time{}, fmt.Errorf("from: %w", err)
		}
		from = t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return "", time.Time{}, time.Time{}, fmt.Errorf("to: %w", err)
		}
		to = t
	}
	if !to.After(from) {
		return "", time.Time{}, time.Time{}, errors.New("to must be after from")
	}
	return fence, from, to, nil
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log().Error("request failed", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// --- wire types -------------------------------------------------------------

type fenceJSON struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	RadiusM int     `json:"radius_m"`
}

func fenceOf(f store.Fence) fenceJSON {
	return fenceJSON{ID: f.ID, Name: f.Name, Lat: f.At.Lat, Lon: f.At.Lon, RadiusM: f.RadiusM}
}

type watchJSON struct {
	ID        string     `json:"id"`
	Device    string     `json:"device"`
	Kind      string     `json:"kind"`
	FenceID   string     `json:"fence_id"`
	Operators []string   `json:"operators"`
	MinRangeM int        `json:"min_range_m"`
	Threshold int        `json:"threshold,omitempty"`
	Repeat    bool       `json:"repeat"`
	State     string     `json:"state"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	FiredAt   *time.Time `json:"fired_at,omitempty"`
	Baseline  int        `json:"baseline_size"`
}

func watchOf(w *store.Watch) watchJSON {
	return watchJSON{
		ID: w.ID, Device: w.Device, Kind: string(w.Kind), FenceID: w.FenceID,
		Operators: w.OperatorKeys, MinRangeM: w.MinRangeM, Threshold: w.Threshold,
		Repeat: w.Repeat, State: string(w.State), CreatedAt: w.CreatedAt,
		ExpiresAt: w.ExpiresAt, FiredAt: w.FiredAt, Baseline: len(w.Baseline),
	}
}

func vehiclesOf(vs []entur.Vehicle) []map[string]any {
	out := make([]map[string]any, 0, len(vs))
	for _, v := range vs {
		out = append(out, map[string]any{
			"id": v.ID, "operator": v.Operator, "operator_key": v.OperatorKey,
			"distance_m": int(v.DistanceM + 0.5), "bearing": v.Compass(),
			"range_km": float64(v.RangeM) / 1000, "battery_pct": v.FuelPct,
			"app_link": v.AppLinkIOS,
		})
	}
	return out
}

// --- helpers ----------------------------------------------------------------

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func coords(latS, lonS string) (float64, float64, error) {
	if latS == "" || lonS == "" {
		return 0, 0, errors.New("lat and lon are required")
	}
	lat, err := strconv.ParseFloat(latS, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("lat: %w", err)
	}
	lon, err := strconv.ParseFloat(lonS, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("lon: %w", err)
	}
	return lat, lon, checkCoords(lat, lon)
}

func checkCoords(lat, lon float64) error {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return errors.New("coordinates out of range")
	}
	return nil
}

func intParam(q map[string][]string, key string, def int) (int, error) {
	vs, ok := q[key]
	if !ok || len(vs) == 0 || vs[0] == "" {
		return def, nil
	}
	n, err := strconv.Atoi(vs[0])
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must not be negative", key)
	}
	return n, nil
}

func splitOperators(s string) []string {
	if s == "" || strings.EqualFold(s, "all") {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// validOperators rejects unknown keys here rather than letting them reach the
// upstream, which answers an unrecognised operator with an empty list.
func validOperators(keys []string) error {
	_, err := entur.OperatorIDs(keys)
	return err
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func orRandom(s string) string {
	if strings.TrimSpace(s) == "" {
		return randomID()
	}
	return s
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; if it does, a time-based id
		// is still unique enough for a local database.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// status answers the whole question in one call: what is inside the fence
// right now, and for anything that is not, how far away the nearest one is.
//
// A bare zero is a bad answer. "No Ryde here" and "no Ryde within five
// kilometres" call for completely different decisions, and so does "none here,
// but one 173 m away" - which is a walk, not a defeat.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fenceID := r.URL.Query().Get("fence")
	if fenceID == "" {
		writeErr(w, http.StatusBadRequest, "fence is required")
		return
	}
	f, err := s.Store.Fence(ctx, fenceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such fence")
		return
	}
	want := splitOperators(r.URL.Query().Get("operators"))
	if err := validOperators(want); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	counts, at, err := s.Store.LatestCounts(ctx, fenceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	nearest, err := s.Store.LatestNearest(ctx, fenceID)
	if err != nil {
		s.fail(w, err)
		return
	}

	type nearestJSON struct {
		DistanceM *int      `json:"distance_m"`
		VehicleID string    `json:"vehicle_id,omitempty"`
		At        time.Time `json:"at"`
	}
	outCounts := map[string]int{}
	outNearest := map[string]nearestJSON{}
	for _, o := range entur.Operators() {
		if len(want) > 0 && !contains(want, o.Key) {
			continue
		}
		outCounts[o.Key] = counts[o.Key]
		if n, ok := nearest[o.Key]; ok {
			outNearest[o.Key] = nearestJSON{
				DistanceM: n.DistanceM, VehicleID: n.VehicleID, At: n.At,
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fence":   fenceOf(f),
		"at":      at,
		"counts":  outCounts,
		"nearest": outNearest,
		"summary": summarise(want, counts, nearest),
	})
}

// summarise renders the human sentence: "2 Bolt available, 173 m to nearest
// Voi, 481 m to nearest Ryde".
//
// Operators that are present come first, because an available scooter beats
// any distance; the rest follow in order of how far you would have to walk.
func summarise(want []string, counts map[string]int, nearest map[string]store.Nearest) string {
	type entry struct {
		text  string
		here  bool
		order float64
	}
	var entries []entry

	for _, o := range entur.Operators() {
		if len(want) > 0 && !contains(want, o.Key) {
			continue
		}
		if n := counts[o.Key]; n > 0 {
			entries = append(entries, entry{
				text:  fmt.Sprintf("%d %s available", n, o.Name),
				here:  true,
				order: -float64(n),
			})
			continue
		}
		near, ok := nearest[o.Key]
		if !ok {
			continue
		}
		if near.DistanceM == nil {
			entries = append(entries, entry{
				text:  fmt.Sprintf("no %s nearby", o.Name),
				order: math.Inf(1),
			})
			continue
		}
		entries = append(entries, entry{
			text:  fmt.Sprintf("%d m to nearest %s", *near.DistanceM, o.Name),
			order: float64(*near.DistanceM),
		})
	}
	if len(entries) == 0 {
		return "nothing known yet"
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].here != entries[j].here {
			return entries[i].here
		}
		return entries[i].order < entries[j].order
	})
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.text)
	}
	return strings.Join(parts, ", ")
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
