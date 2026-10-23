package main

import (
	"net/http"
	"strings"
)

// withIngestCORS lets browser builds (the HTML5 build on itch.io) call the
// keyed ingest routes. The preflight is answered with 204 before rate
// limiting and auth, because browsers never send X-API-Key on OPTIONS.
// Ingest uses no cookies, so reflecting an allowlisted origin is enough.
func withIngestCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		h := w.Header()
		h.Add("Vary", "Origin")
		if ingestOriginAllowed(origin) {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
			h.Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ingestOriginAllowed: the itch hosts (itchOrigin), itch's CDN *.hwcdn.net,
// cynthiongame.com / runedpoodle.com, and local dev servers.
func ingestOriginAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	if itchOrigin(origin) {
		return true
	}
	switch origin {
	case "https://cynthiongame.com", "https://www.cynthiongame.com",
		"https://runedpoodle.com", "https://www.runedpoodle.com":
		return true
	}
	if strings.HasPrefix(origin, "http://127.0.0.1:") || strings.HasPrefix(origin, "http://localhost:") {
		return true
	}
	if !strings.HasPrefix(origin, "https://") {
		return false
	}
	host := strings.TrimPrefix(origin, "https://")
	if i := strings.IndexAny(host, ":/"); i >= 0 {
		host = host[:i]
	}
	return strings.HasSuffix(host, ".hwcdn.net")
}
