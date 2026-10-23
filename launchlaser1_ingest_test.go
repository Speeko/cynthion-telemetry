package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

const testLaunchLaser1Key = "launchlaser1-ingest-key-24!!"

func newLL1TestServer(t *testing.T, key string) (*server, string) {
	t.Helper()
	s, ts := newIngestTestServer(t, testManRocketKey)
	s.cfg.LaunchLaser1APIKey = key
	return s, ts.URL
}

func ll1Count(t *testing.T, s *server, table string) int {
	t.Helper()
	switch table {
	case "launchlaser1_events", "launchlaser1_crashes", "manrocket_events", "manrocket_crashes", "events", "crashes":
	default:
		t.Fatalf("unknown table %q", table)
	}
	var n int
	if err := s.store.db.QueryRow("SELECT COUNT(1) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestLaunchLaser1EventsAndCrashIsolated(t *testing.T) {
	s, url := newLL1TestServer(t, testLaunchLaser1Key)
	status, raw := postJSON(t, url+"/v1/launchlaser1/events", testLaunchLaser1Key, sampleBatch(3))
	if status != http.StatusOK {
		t.Fatalf("events status=%d body=%s", status, raw)
	}
	var resp struct {
		OK       bool `json:"ok"`
		Received int  `json:"received"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || !resp.OK || resp.Received != 3 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	crash := Crash{InstallID: "ll1-install", SessionID: "s", AppVersion: "1.0", OS: "Android 14", ErrorSummary: "boom", Payload: json.RawMessage(`{"k":1}`)}
	status, raw = postJSON(t, url+"/v1/launchlaser1/crash", testLaunchLaser1Key, crash)
	if status != http.StatusOK {
		t.Fatalf("crash status=%d body=%s", status, raw)
	}
	if ll1Count(t, s, "launchlaser1_events") != 3 || ll1Count(t, s, "launchlaser1_crashes") != 1 {
		t.Fatal("launchlaser1 rows missing")
	}
	for _, other := range []string{"manrocket_events", "manrocket_crashes", "events", "crashes"} {
		if n := ll1Count(t, s, other); n != 0 {
			t.Fatalf("%s=%d, want 0", other, n)
		}
	}
	var payload string
	if err := s.store.db.QueryRow(`SELECT payload FROM launchlaser1_events LIMIT 1`).Scan(&payload); err != nil || payload != `{"origin":"pad"}` {
		t.Fatalf("payload=%q err=%v", payload, err)
	}
}

func TestLaunchLaser1Auth(t *testing.T) {
	s, url := newLL1TestServer(t, testLaunchLaser1Key)
	for _, path := range []string{"/v1/launchlaser1/events", "/v1/launchlaser1/crash"} {
		for _, key := range []string{"", "wrong-key-wrong-key-wrong!!", testManRocketKey, testCynthionKey} {
			if status, raw := postJSON(t, url+path, key, sampleBatch(1)); status != http.StatusUnauthorized {
				t.Errorf("POST %s key %q status=%d body=%s, want 401", path, key, status, raw)
			}
		}
	}
	// LL1 key opens nothing else.
	for _, path := range []string{"/v1/manrocket/events", "/v1/manrocket/crash", "/v1/events", "/v1/crash"} {
		if status, _ := postJSON(t, url+path, testLaunchLaser1Key, sampleBatch(1)); status != http.StatusUnauthorized {
			t.Errorf("POST %s with LL1 key status=%d, want 401", path, status)
		}
	}
	if ll1Count(t, s, "launchlaser1_events")+ll1Count(t, s, "launchlaser1_crashes")+ll1Count(t, s, "manrocket_events")+ll1Count(t, s, "events") != 0 {
		t.Fatal("unauthorized requests wrote rows")
	}
	// Unset or short configured key rejects even an exact match.
	for _, key := range []string{"", "too-short"} {
		s2, url2 := newLL1TestServer(t, key)
		if status, _ := postJSON(t, url2+"/v1/launchlaser1/events", key, sampleBatch(1)); status != http.StatusUnauthorized {
			t.Fatalf("configured key %q status=%d", key, status)
		}
		if ll1Count(t, s2, "launchlaser1_events") != 0 {
			t.Fatal("short key wrote a row")
		}
	}
}

func TestLaunchLaser1Validation(t *testing.T) {
	s, url := newLL1TestServer(t, testLaunchLaser1Key)
	if status, _ := postJSON(t, url+"/v1/launchlaser1/events", testLaunchLaser1Key, EventsBatch{Events: []Event{{EventType: "x"}}}); status != http.StatusBadRequest {
		t.Fatalf("missing install_id status=%d", status)
	}
	if status, _ := postJSON(t, url+"/v1/launchlaser1/events", testLaunchLaser1Key, sampleBatch(maxEventsPerBatch+1)); status != http.StatusBadRequest {
		t.Fatalf("oversize batch status=%d", status)
	}
	if status, _ := postJSON(t, url+"/v1/launchlaser1/crash", testLaunchLaser1Key, Crash{}); status != http.StatusBadRequest {
		t.Fatalf("crash missing install_id status=%d", status)
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/v1/launchlaser1/events", nil)
	req.Header.Set("X-API-Key", testLaunchLaser1Key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405 (no read route)", resp.StatusCode)
	}
	if status, _ := postJSON(t, url+"/v1/launchlaser1/bugreport", testLaunchLaser1Key, sampleBatch(1)); status != http.StatusNotFound {
		t.Fatalf("bugreport status=%d, want 404", status)
	}
	if ll1Count(t, s, "launchlaser1_events")+ll1Count(t, s, "launchlaser1_crashes") != 0 {
		t.Fatal("validation failures wrote rows")
	}
}

func TestLaunchLaser1CORS(t *testing.T) {
	_, url := newLL1TestServer(t, testLaunchLaser1Key)
	origin := "https://html-classic.itch.zone"
	for _, path := range []string{"/v1/launchlaser1/events", "/v1/launchlaser1/crash"} {
		req, _ := http.NewRequest(http.MethodOptions, url+path, nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "content-type,x-api-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("%s preflight status=%d acao=%q", path, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
	}
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(sampleBatch(1))
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/launchlaser1/events", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", testLaunchLaser1Key)
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("post status=%d acao=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}
