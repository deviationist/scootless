package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const allowed = "https://board.example.com"

func corsServer() *Server {
	return &Server{Token: "sekret", AllowedOrigins: []string{allowed}}
}

func corsDo(t *testing.T, s *Server, method, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	req := httptest.NewRequest(method, path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.cors(s.authenticated(inner)).ServeHTTP(rec, req)
	return rec
}

// The regression this whole file exists for: a preflight carries no
// Authorization header, so it must be answered before the bearer check.
func TestPreflightIsNotRejectedByAuth(t *testing.T) {
	rec := corsDo(t, corsServer(), http.MethodOptions, "/api/v1/board", map[string]string{
		"Origin":                        allowed,
		"Access-Control-Request-Method": "GET",
	})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight got %d, want 204 — auth ran before CORS", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Fatal("preflight must advertise Authorization as an allowed header")
	}
}

func TestPreflightEchoesTheExactOrigin(t *testing.T) {
	rec := corsDo(t, corsServer(), http.MethodOptions, "/api/v1/board", map[string]string{
		"Origin":                        allowed,
		"Access-Control-Request-Method": "GET",
	})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowed {
		t.Fatalf("got %q, want the exact origin (never \"*\", which bars Authorization)", got)
	}
}

func TestUnknownOriginIsRefused(t *testing.T) {
	rec := corsDo(t, corsServer(), http.MethodOptions, "/api/v1/board", map[string]string{
		"Origin":                        "https://evil.test",
		"Access-Control-Request-Method": "GET",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// Prefix matching would admit https://board.example.com.evil.test.
func TestOriginMatchIsExactNotPrefix(t *testing.T) {
	s := corsServer()
	if s.originAllowed(allowed + ".evil.test") {
		t.Fatal("origin matching must be exact")
	}
}

func TestAuthStillAppliesToRealRequests(t *testing.T) {
	rec := corsDo(t, corsServer(), http.MethodGet, "/api/v1/board", map[string]string{"Origin": allowed})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 — CORS must not become an auth bypass", rec.Code)
	}
}

func TestAuthorisedCrossOriginRequestIsAnnotated(t *testing.T) {
	rec := corsDo(t, corsServer(), http.MethodGet, "/api/v1/board", map[string]string{
		"Origin": allowed, "Authorization": "Bearer sekret",
	})
	if rec.Code != http.StatusTeapot {
		t.Fatalf("got %d, want the handler to run", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != allowed {
		t.Fatal("a real cross-origin response must carry the header too, not just the preflight")
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Fatal("Vary: Origin is required so caches cannot cross-serve")
	}
}

func TestNoOriginConfiguredLeavesBehaviourUnchanged(t *testing.T) {
	s := &Server{Token: "sekret"}
	rec := corsDo(t, s, http.MethodGet, "/api/v1/board", map[string]string{"Authorization": "Bearer sekret"})
	if rec.Code != http.StatusTeapot {
		t.Fatalf("got %d, want the loopback deployment to be untouched", rec.Code)
	}
}
