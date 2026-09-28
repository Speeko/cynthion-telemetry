package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testCynthionKey  = "cynthion-ingest-key-24ch!!"
	testManRocketKey = "manrocket-ingest-key-24c!!"
)

func newIngestTestServer(t *testing.T, manRocketKey string) (*server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	store, err := openStore(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	bugDir := filepath.Join(dir, "bugreports")
	mrBugDir := filepath.Join(dir, "manrocket_bugreports")
	if err := os.MkdirAll(bugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mrBugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := &server{
		cfg: Config{
			APIKey:          testCynthionKey,
			ManRocketAPIKey: manRocketKey,
		},
		store:           store,
		limiter:         newIPLimiter(1000, 10000),
		bugReportDir:    bugDir,
		manrocketBugDir: mrBugDir,
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func countTable(t *testing.T, s *server, table string) int {
	t.Helper()
	switch table {
	case "events", "crashes", "bugreports",
		"manrocket_events", "manrocket_crashes", "manrocket_bugreports",
		"manrocket_levels":
	default:
		t.Fatalf("refusing to count unknown table %q", table)
	}
	var n int
	if err := s.store.db.QueryRow("SELECT COUNT(1) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func postJSON(t *testing.T, url, apiKey string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func sampleBatch(n int) EventsBatch {
	ev := make([]Event, n)
	for i := range ev {
		ev[i] = Event{
			ClientTS:  1779515374000,
			EventType: "session_start",
			Payload:   json.RawMessage(`{"origin":"pad"}`),
		}
	}
	return EventsBatch{
		InstallID:   "install-mr-1",
		SessionID:   "session-mr-1",
		AppVersion:  "0.9.0",
		OS:          "Windows 10",
		GPU:         "NVIDIA RTX 3080",
		SteamID:     "76561198000000000",
		PersonaName: "Pilot",
		Country:     "US",
		Language:    "english",
		Events:      ev,
	}
}

func TestManRocketEventsIsolatedFromCynthion(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)

	status, raw := postJSON(t, ts.URL+"/v1/manrocket/events", testManRocketKey, sampleBatch(2))
	if status != http.StatusOK {
		t.Fatalf("manrocket events status=%d body=%s", status, raw)
	}
	var resp struct {
		OK       bool `json:"ok"`
		Received int  `json:"received"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.Received != 2 {
		t.Fatalf("response = %+v", resp)
	}
	if got := countTable(t, s, "manrocket_events"); got != 2 {
		t.Fatalf("manrocket_events = %d, want 2", got)
	}
	if got := countTable(t, s, "events"); got != 0 {
		t.Fatalf("cynthion events = %d, want 0", got)
	}

	var steam, persona, country, language, payload string
	err := s.store.db.QueryRow(`SELECT steam_id, persona_name, country, language, payload FROM manrocket_events LIMIT 1`).
		Scan(&steam, &persona, &country, &language, &payload)
	if err != nil {
		t.Fatal(err)
	}
	if steam != "76561198000000000" || persona != "Pilot" || country != "US" || language != "english" || payload != `{"origin":"pad"}` {
		t.Fatalf("stored row steam=%q persona=%q country=%q language=%q payload=%q", steam, persona, country, language, payload)
	}

	// The same body on Cynthion ingest stays in the Cynthion table.
	status, raw = postJSON(t, ts.URL+"/v1/events", testCynthionKey, sampleBatch(1))
	if status != http.StatusOK {
		t.Fatalf("cynthion events status=%d body=%s", status, raw)
	}
	if got := countTable(t, s, "events"); got != 1 {
		t.Fatalf("cynthion events = %d, want 1", got)
	}
	if got := countTable(t, s, "manrocket_events"); got != 2 {
		t.Fatalf("manrocket_events = %d, want 2 (cynthion post must not write it)", got)
	}
}

func TestManRocketIngestAuth(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	batch := sampleBatch(1)
	paths := []string{"/v1/manrocket/events", "/v1/manrocket/crash"}

	for _, path := range paths {
		for _, key := range []string{"", "nope-not-the-manrocket-key!", testCynthionKey} {
			status, raw := postJSON(t, ts.URL+path, key, batch)
			if status != http.StatusUnauthorized {
				t.Errorf("POST %s key %q status=%d body=%s, want 401", path, key, status, raw)
			}
		}
	}
	if countTable(t, s, "manrocket_events")+countTable(t, s, "manrocket_crashes")+countTable(t, s, "events")+countTable(t, s, "crashes") != 0 {
		t.Fatal("unauthorized requests wrote rows")
	}

	// ManRocket key must not open Cynthion ingest.
	status, raw := postJSON(t, ts.URL+"/v1/events", testManRocketKey, batch)
	if status != http.StatusUnauthorized {
		t.Fatalf("cynthion events with manrocket key status=%d body=%s", status, raw)
	}
	if countTable(t, s, "events") != 0 {
		t.Fatal("manrocket key wrote a cynthion event")
	}

	// Unset or short key rejects even an exact header match.
	for _, key := range []string{"", "too-short"} {
		s2, ts2 := newIngestTestServer(t, key)
		status, raw := postJSON(t, ts2.URL+"/v1/manrocket/events", key, batch)
		if status != http.StatusUnauthorized {
			t.Fatalf("configured key %q status=%d body=%s", key, status, raw)
		}
		if countTable(t, s2, "manrocket_events") != 0 {
			t.Fatal("short key wrote a row")
		}
	}
}

func TestManRocketEventsValidation(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)

	status, raw := postJSON(t, ts.URL+"/v1/manrocket/events", testManRocketKey, EventsBatch{Events: []Event{{EventType: "x"}}})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "install_id required") {
		t.Fatalf("missing install_id status=%d body=%s", status, raw)
	}

	status, raw = postJSON(t, ts.URL+"/v1/manrocket/events", testManRocketKey, sampleBatch(maxEventsPerBatch+1))
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "too many events") {
		t.Fatalf("oversize batch status=%d body=%s", status, raw)
	}

	status, raw = postJSON(t, ts.URL+"/v1/manrocket/events", testManRocketKey, sampleBatch(0))
	if status != http.StatusOK {
		t.Fatalf("empty batch status=%d body=%s", status, raw)
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/manrocket/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", testManRocketKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", resp.StatusCode)
	}
	if countTable(t, s, "manrocket_events") != 0 || countTable(t, s, "events") != 0 {
		t.Fatal("validation failures wrote rows")
	}
}

func TestManRocketCrashIsolated(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	body := Crash{
		InstallID:    "install-mr-crash",
		SessionID:    "session-mr-crash",
		AppVersion:   "0.9.0",
		OS:           "Linux",
		GPU:          "NVIDIA RTX 3080 (Vulkan)",
		ErrorSummary: "SIGSEGV at pad",
		BootLog:      "boot",
		PlayerLog:    "player",
		Payload:      json.RawMessage(`{"kind":"crash"}`),
	}
	status, raw := postJSON(t, ts.URL+"/v1/manrocket/crash", testManRocketKey, body)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var resp struct {
		OK bool  `json:"ok"`
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID == 0 {
		t.Fatalf("response = %+v", resp)
	}
	if countTable(t, s, "manrocket_crashes") != 1 || countTable(t, s, "crashes") != 0 {
		t.Fatalf("crashes mr=%d cynthion=%d", countTable(t, s, "manrocket_crashes"), countTable(t, s, "crashes"))
	}

	status, raw = postJSON(t, ts.URL+"/v1/crash", testCynthionKey, body)
	if status != http.StatusOK {
		t.Fatalf("cynthion crash status=%d body=%s", status, raw)
	}
	if countTable(t, s, "crashes") != 1 || countTable(t, s, "manrocket_crashes") != 1 {
		t.Fatal("crash tables mixed")
	}

	status, _ = postJSON(t, ts.URL+"/v1/manrocket/crash", testManRocketKey, Crash{})
	if status != http.StatusBadRequest {
		t.Fatalf("missing install_id status=%d", status)
	}
}

func TestManRocketBugReportIsolated(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	const zipBody = "not-a-real-zip-but-bytes"

	status, raw := postMultipart(t, ts.URL+"/v1/manrocket/bugreport", testManRocketKey, map[string]string{
		"install_id":        "install-mr-bug",
		"session_id":        "session-mr-bug",
		"app_version":       "0.9.0",
		"os":                "Windows 10",
		"gpu":               "Intel UHD",
		"category":          "gameplay",
		"severity":          "high",
		"description":       "rocket stuck on pad",
		"expected_behavior": "liftoff",
	}, zipBody)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var resp struct {
		OK      bool   `json:"ok"`
		ID      int64  `json:"id"`
		Archive string `json:"archive"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID == 0 || resp.Archive == "" {
		t.Fatalf("response = %+v raw=%s", resp, raw)
	}
	saved, err := os.ReadFile(filepath.Join(s.manrocketBugDir, resp.Archive))
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != zipBody {
		t.Fatalf("archive bytes = %q", saved)
	}
	if n, _ := os.ReadDir(s.bugReportDir); len(n) != 0 {
		t.Fatalf("cynthion bugreport dir has %d files", len(n))
	}
	if countTable(t, s, "manrocket_bugreports") != 1 || countTable(t, s, "bugreports") != 0 {
		t.Fatal("bugreport tables mixed after manrocket upload")
	}
	var category, description string
	var size int64
	if err := s.store.db.QueryRow(`SELECT category, description, archive_size FROM manrocket_bugreports`).Scan(&category, &description, &size); err != nil {
		t.Fatal(err)
	}
	if category != "gameplay" || description != "rocket stuck on pad" || size != int64(len(zipBody)) {
		t.Fatalf("row category=%q description=%q size=%d", category, description, size)
	}

	// Cynthion bugreport still lands in its own table and directory.
	status, raw = postMultipart(t, ts.URL+"/v1/bugreport", testCynthionKey, map[string]string{
		"install_id": "install-cy-bug",
		"category":   "render",
	}, "cynthion-zip")
	if status != http.StatusOK {
		t.Fatalf("cynthion bugreport status=%d body=%s", status, raw)
	}
	if countTable(t, s, "bugreports") != 1 || countTable(t, s, "manrocket_bugreports") != 1 {
		t.Fatal("bugreport tables mixed after cynthion upload")
	}
	if n, _ := os.ReadDir(s.bugReportDir); len(n) != 1 {
		t.Fatalf("cynthion bugreport dir has %d files", len(n))
	}
	if n, _ := os.ReadDir(s.manrocketBugDir); len(n) != 1 {
		t.Fatalf("manrocket bugreport dir has %d files", len(n))
	}

	// Auth and required fields.
	status, _ = postMultipart(t, ts.URL+"/v1/manrocket/bugreport", testCynthionKey, map[string]string{
		"install_id": "install-mr-bug",
	}, zipBody)
	if status != http.StatusUnauthorized {
		t.Fatalf("cynthion key on manrocket bugreport status=%d", status)
	}
	status, raw = postMultipart(t, ts.URL+"/v1/manrocket/bugreport", testManRocketKey, map[string]string{}, zipBody)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "install_id required") {
		t.Fatalf("missing install_id status=%d body=%s", status, raw)
	}
	status, raw = postMultipartNoFile(t, ts.URL+"/v1/manrocket/bugreport", testManRocketKey, map[string]string{
		"install_id": "install-mr-bug",
	})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "archive file required") {
		t.Fatalf("missing archive status=%d body=%s", status, raw)
	}
	if countTable(t, s, "manrocket_bugreports") != 1 || countTable(t, s, "bugreports") != 1 {
		t.Fatal("rejected bugreports wrote rows")
	}
}

func TestManRocketCommunityRoutesStayPublic(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)

	resp, err := http.Get(ts.URL + "/v1/manrocket/levels")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("levels status=%d body=%s", resp.StatusCode, raw)
	}
	if resp.Header.Get("X-API-Key") != "" {
		t.Fatal("community response should not echo an API key")
	}
	var listed struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || !listed.OK {
		t.Fatalf("levels body=%s", raw)
	}

	// Uploading a community level still does not require the ingest key
	// and does not land in the analytics tables.
	status, raw := postJSON(t, ts.URL+"/v1/manrocket/levels", "", map[string]any{
		"id":         "course-1",
		"title":      "Pad",
		"author":     "ada",
		"install_id": "install-community",
	})
	if status != http.StatusOK {
		t.Fatalf("level upload status=%d body=%s", status, raw)
	}
	if countTable(t, s, "manrocket_levels") != 1 {
		t.Fatal("community level was not stored")
	}
	if countTable(t, s, "manrocket_events")+countTable(t, s, "events") != 0 {
		t.Fatal("community level wrote an analytics event")
	}

	health, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status=%d", health.StatusCode)
	}
}

func postMultipart(t *testing.T, url, apiKey string, fields map[string]string, archive string) (int, []byte) {
	t.Helper()
	return postMultipartMaybeFile(t, url, apiKey, fields, true, archive)
}

func postMultipartNoFile(t *testing.T, url, apiKey string, fields map[string]string) (int, []byte) {
	t.Helper()
	return postMultipartMaybeFile(t, url, apiKey, fields, false, "")
}

func postMultipartMaybeFile(t *testing.T, url, apiKey string, fields map[string]string, withFile bool, archive string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if withFile {
		fw, err := w.CreateFormFile("archive", "report.zip")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(fw, archive); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func TestManRocketEventsBatchLimitAccepted(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	status, raw := postJSON(t, ts.URL+"/v1/manrocket/events", testManRocketKey, sampleBatch(maxEventsPerBatch))
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if got := countTable(t, s, "manrocket_events"); got != maxEventsPerBatch {
		t.Fatalf("stored %d, want %d", got, maxEventsPerBatch)
	}
	if countTable(t, s, "events") != 0 {
		t.Fatal("batch limit write leaked into cynthion events")
	}
}
