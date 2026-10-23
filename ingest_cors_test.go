package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func preflight(t *testing.T, url, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type,x-api-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestManRocketIngestCORSPreflight(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	for _, path := range []string{"/v1/manrocket/events", "/v1/manrocket/crash", "/v1/manrocket/bugreport"} {
		for _, origin := range []string{"https://html-classic.itch.zone", "https://v6p9d9t4.ssl.hwcdn.net", "https://itch.io", "https://brett.itch.io"} {
			resp := preflight(t, ts.URL+path, origin)
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("%s %s: status=%d, want 204 (preflight must skip auth)", path, origin, resp.StatusCode)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
				t.Fatalf("%s %s: ACAO=%q", path, origin, got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, X-API-Key" {
				t.Fatalf("%s: allow-headers=%q", path, got)
			}
			if got := resp.Header.Get("Access-Control-Allow-Methods"); got != "POST, OPTIONS" {
				t.Fatalf("%s: allow-methods=%q", path, got)
			}
			if got := resp.Header.Get("Access-Control-Max-Age"); got != "86400" {
				t.Fatalf("%s: max-age=%q", path, got)
			}
		}
		resp := preflight(t, ts.URL+path, "https://evil.example")
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("%s: unknown origin got ACAO=%q", path, got)
		}
	}
}

func TestManRocketIngestCORSOnPost(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	origin := "https://html-classic.itch.zone"
	do := func(key string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/manrocket/events", jsonBody(t, sampleBatch(2)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp := do(testManRocketKey)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Fatalf("post ACAO=%q", got)
	}
	if n := countTable(t, s, "manrocket_events"); n != 2 {
		t.Fatalf("manrocket_events=%d, want 2", n)
	}
	// auth still enforced on the real request
	if resp := do(""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-key post status=%d, want 401", resp.StatusCode)
	}
}
