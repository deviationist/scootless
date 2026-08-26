package api

import (
	"net/http"
	"strings"
)

// CORS exists because a browser is now a first-class client.
//
// The subtlety worth remembering: a preflight OPTIONS carries no Authorization
// header — browsers deliberately strip credentials from it. So CORS must be
// the OUTERMOST middleware and must answer preflights itself. Wrapping it
// inside the bearer check instead produces a service that works perfectly from
// curl and fails from every browser, with a 401 on a request the developer
// never wrote.
//
// The allowed origin is echoed back exactly rather than answered with "*",
// because "*" is invalid for credentialed requests and silently rules out the
// Authorization header this API depends on.

// isPreflight reports whether r is a CORS preflight rather than a real request.
func isPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
}

// originAllowed matches an Origin against the configured allowlist. Matching is
// exact: an origin is scheme + host + port, and prefix matching here is how
// "https://board.example.com.evil.test" gets let in.
func (s *Server) originAllowed(origin string) bool {
	for _, a := range s.AllowedOrigins {
		if strings.EqualFold(strings.TrimSpace(a), origin) {
			return true
		}
	}
	return false
}

// cors handles preflight and annotates cross-origin responses. With no
// configured origins it is inert, which keeps the loopback-only deployment
// exactly as it was.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		if origin == "" {
			// Not a browser cross-origin request at all — curl, the CLI, a
			// same-origin fetch. Nothing to negotiate.
			next.ServeHTTP(w, r)
			return
		}

		// The response now depends on Origin, allowed or not, so a shared cache
		// must not serve one origin's copy to another.
		w.Header().Add("Vary", "Origin")

		if !s.originAllowed(origin) {
			if isPreflight(r) {
				// Refuse the preflight outright rather than letting an
				// unannotated 204 look like success.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			// Serve it, but without the header: the browser blocks the read.
			// A non-browser client with a stray Origin still works.
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)

		if isPreflight(r) {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
