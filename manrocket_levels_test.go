package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func uploadMRLevel(t *testing.T, base, id, title, install string) {
	t.Helper()
	status, raw := postJSON(t, base+"/v1/manrocket/levels", "", map[string]any{
		"id": id, "title": title, "author": "ada", "install_id": install,
	})
	if status != http.StatusOK {
		t.Fatalf("upload %s: %d %s", id, status, raw)
	}
}

func playMR(t *testing.T, base, id, phase, install, session string) (int, map[string]any, []byte) {
	t.Helper()
	status, raw := postJSON(t, base+"/v1/manrocket/levels/"+id+"/play", "", map[string]any{
		"phase": phase, "install_id": install, "session_id": session,
	})
	var body map[string]any
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("play json: %v %s", err, raw)
		}
	}
	return status, body, raw
}

func getMR(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("get json %s: %v %s", url, err, raw)
	}
	return resp.StatusCode, body
}

func levelIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["levels"].([]any)
	if !ok {
		t.Fatalf("levels = %#v", body["levels"])
	}
	ids := make([]string, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("level = %#v", item)
		}
		id, _ := m["id"].(string)
		ids = append(ids, id)
		if _, ok := m["play_count"].(float64); !ok {
			t.Fatalf("level %s missing play_count: %#v", id, m["play_count"])
		}
		if _, ok := m["completion_count"].(float64); !ok {
			t.Fatalf("level %s missing completion_count: %#v", id, m["completion_count"])
		}
	}
	return ids
}

func TestManRocketLevelPlayCountsAndSort(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	base := ts.URL

	uploadMRLevel(t, base, "alpha", "Alpha", "owner-a")
	uploadMRLevel(t, base, "beta", "Beta", "owner-b")
	uploadMRLevel(t, base, "gamma", "Gamma", "owner-c")
	uploadMRLevel(t, base, "delta", "Delta", "owner-d")

	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET received_at=? WHERE id=?`, 1000, "alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET received_at=? WHERE id=?`, 2000, "gamma"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET received_at=? WHERE id=?`, 2500, "delta"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET received_at=? WHERE id=?`, 3000, "beta"); err != nil {
		t.Fatal(err)
	}

	status, body, raw := playMR(t, base, "alpha", "start", "pilot-1", "sess-a1")
	if status != http.StatusOK || body["counted"] != true {
		t.Fatalf("first start: %d %s", status, raw)
	}
	status, body, raw = playMR(t, base, "alpha", "start", "pilot-1", "sess-a1")
	if status != http.StatusOK || body["counted"] != false || body["play_count"] != float64(1) {
		t.Fatalf("retry start: %d %s", status, raw)
	}
	// Same session from another install is still the same play.
	status, body, raw = playMR(t, base, "alpha", "start", "pilot-2", "sess-a1")
	if status != http.StatusOK || body["counted"] != false || body["play_count"] != float64(1) {
		t.Fatalf("cross-install retry: %d %s", status, raw)
	}
	status, body, raw = playMR(t, base, "alpha", "start", "pilot-1", "sess-a2")
	if status != http.StatusOK || body["counted"] != true || body["play_count"] != float64(2) || body["completion_count"] != float64(0) {
		t.Fatalf("second session: %d %s", status, raw)
	}
	status, body, raw = playMR(t, base, "alpha", "complete", "pilot-1", "sess-a1")
	if status != http.StatusOK || body["counted"] != true || body["play_count"] != float64(2) || body["completion_count"] != float64(1) {
		t.Fatalf("complete: %d %s", status, raw)
	}
	status, body, raw = playMR(t, base, "alpha", "complete", "pilot-1", "sess-a1")
	if status != http.StatusOK || body["counted"] != false || body["completion_count"] != float64(1) {
		t.Fatalf("retry complete: %d %s", status, raw)
	}
	// Ingest key is ignored; the route stays public like votes.
	status, raw = postJSON(t, base+"/v1/manrocket/levels/beta/play", testManRocketKey, map[string]any{
		"phase": "Start", "install_id": "pilot-1", "session_id": "sess-b1",
	})
	if status != http.StatusOK {
		t.Fatalf("keyed play: %d %s", status, raw)
	}
	for _, spec := range []struct{ id, session string }{
		{"gamma", "sess-g1"},
		{"delta", "sess-d1"},
	} {
		status, _, raw = playMR(t, base, spec.id, "start", "pilot-1", spec.session)
		if status != http.StatusOK {
			t.Fatalf("start %s: %d %s", spec.id, status, raw)
		}
	}
	status, body, raw = playMR(t, base, "gamma", "complete", "pilot-1", "sess-g1")
	if status != http.StatusOK || body["play_count"] != float64(1) || body["completion_count"] != float64(1) {
		t.Fatalf("gamma complete: %d %s", status, raw)
	}

	// Replacing the level must not zero the counters.
	uploadMRLevel(t, base, "alpha", "Alpha 2", "owner-a")

	status, got := getMR(t, base+"/v1/manrocket/levels/alpha")
	if status != http.StatusOK || got["play_count"] != float64(2) || got["completion_count"] != float64(1) || got["votes"] != float64(0) {
		t.Fatalf("get alpha: %d %#v", status, got)
	}

	status, raw = postJSON(t, base+"/v1/manrocket/levels/beta/vote", "", map[string]any{
		"install_id": "voter-1", "value": 1,
	})
	if status != http.StatusOK {
		t.Fatalf("vote: %d %s", status, raw)
	}

	_, plays := getMR(t, base+"/v1/manrocket/levels?sort=plays")
	if ids := levelIDs(t, plays); strings.Join(ids, ",") != "alpha,beta,delta,gamma" {
		t.Fatalf("sort=plays: %v", ids)
	}
	_, top := getMR(t, base+"/v1/manrocket/levels?sort=top")
	_, def := getMR(t, base+"/v1/manrocket/levels")
	_, other := getMR(t, base+"/v1/manrocket/levels?sort=nope")
	wantTop := "beta,delta,gamma,alpha"
	for _, spec := range []struct {
		name string
		body map[string]any
	}{
		{"top", top},
		{"default", def},
		{"unknown", other},
	} {
		if ids := levelIDs(t, spec.body); strings.Join(ids, ",") != wantTop {
			t.Fatalf("sort %s: %v", spec.name, ids)
		}
	}
	_, newest := getMR(t, base+"/v1/manrocket/levels?sort=new")
	if ids := levelIDs(t, newest); strings.Join(ids, ",") != "beta,delta,gamma,alpha" {
		t.Fatalf("sort=new: %v", ids)
	}

	_, page := getMR(t, base+"/v1/manrocket/levels?sort=plays&limit=2&offset=1")
	if ids := levelIDs(t, page); strings.Join(ids, ",") != "beta,delta" {
		t.Fatalf("offset page: %v", ids)
	}
	_, neg := getMR(t, base+"/v1/manrocket/levels?sort=plays&limit=1&offset=-5")
	if ids := levelIDs(t, neg); strings.Join(ids, ",") != "alpha" {
		t.Fatalf("bad offset: %v", ids)
	}
	_, empty := getMR(t, base+"/v1/manrocket/levels?sort=plays&offset=50")
	if ids := levelIDs(t, empty); len(ids) != 0 {
		t.Fatalf("past end: %v", ids)
	}

	if n := countTable(t, s, "manrocket_events"); n != 0 {
		t.Fatalf("play route wrote %d analytics events", n)
	}
	var dedup int
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_level_plays`).Scan(&dedup); err != nil {
		t.Fatal(err)
	}
	// alpha start×2 + alpha complete, beta start, gamma start+complete, delta start.
	if dedup != 7 {
		t.Fatalf("dedup rows = %d, want 7", dedup)
	}
}

func TestManRocketLevelPlayRejects(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	base := ts.URL
	uploadMRLevel(t, base, "alpha", "Alpha", "owner-a")

	cases := []struct {
		path string
		body map[string]any
		want int
		msg  string
	}{
		{"/v1/manrocket/levels/missing/play", map[string]any{"phase": "start", "install_id": "i", "session_id": "s"}, http.StatusNotFound, "not found"},
		{"/v1/manrocket/levels/alpha/play", map[string]any{"phase": "pause", "install_id": "i", "session_id": "s"}, http.StatusBadRequest, "bad phase"},
		{"/v1/manrocket/levels/alpha/play", map[string]any{"phase": "start", "install_id": "", "session_id": "s"}, http.StatusBadRequest, "bad install_id"},
		{"/v1/manrocket/levels/alpha/play", map[string]any{"phase": "start", "install_id": "i", "session_id": ""}, http.StatusBadRequest, "bad session_id"},
		{"/v1/manrocket/levels/alpha/play", map[string]any{"phase": "start", "install_id": strings.Repeat("a", 65), "session_id": "s"}, http.StatusBadRequest, "bad install_id"},
		{"/v1/manrocket/levels/alpha/play", map[string]any{"phase": "complete", "install_id": "i", "session_id": strings.Repeat("b", 65)}, http.StatusBadRequest, "bad session_id"},
	}
	for _, tc := range cases {
		status, raw := postJSON(t, base+tc.path, "", tc.body)
		if status != tc.want || !strings.Contains(string(raw), tc.msg) {
			t.Fatalf("%s %#v: %d %s", tc.path, tc.body["phase"], status, raw)
		}
	}

	req, err := http.NewRequest(http.MethodPut, base+"/v1/manrocket/levels/alpha/play", strings.NewReader(`{"phase":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT play: %d", resp.StatusCode)
	}

	status, got := getMR(t, base+"/v1/manrocket/levels/alpha")
	if status != http.StatusOK || got["play_count"] != float64(0) || got["completion_count"] != float64(0) {
		t.Fatalf("counts after rejects: %d %#v", status, got)
	}
}

func TestManRocketLevelPlayMigratesOldTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE manrocket_levels (
		id TEXT PRIMARY KEY,
		received_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		title TEXT NOT NULL,
		author TEXT NOT NULL,
		install_id TEXT NOT NULL,
		votes INTEGER NOT NULL DEFAULT 0,
		payload TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO manrocket_levels
		(id, received_at, updated_at, title, author, install_id, votes, payload)
		VALUES ('old', 1, 1, 'Old', 'ada', 'inst-1', 3, '{"id":"old"}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	var plays, completions, votes int
	if err := store.db.QueryRow(
		`SELECT play_count, completion_count, votes FROM manrocket_levels WHERE id='old'`,
	).Scan(&plays, &completions, &votes); err != nil {
		t.Fatal(err)
	}
	if plays != 0 || completions != 0 || votes != 3 {
		t.Fatalf("migrated row = plays %d completions %d votes %d", plays, completions, votes)
	}
	var idx string
	if err := store.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_mr_levels_plays'`,
	).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"manrocket_level_milestones", "manrocket_level_notifications"} {
		var name string
		if err := store.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
}

func TestCrossedPlayMilestones(t *testing.T) {
	cases := []struct {
		prev, next int
		want       string
	}{
		{0, 1, "1"},
		{1, 2, ""},
		{9, 10, "10"},
		{10, 11, ""},
		{99, 100, "100"},
		{499, 500, "500"},
		{500, 501, ""},
		{0, 100, "1,10,100"},
		{10, 500, "100,500"},
	}
	for _, tc := range cases {
		got := mrCrossedPlayMilestones(tc.prev, tc.next)
		parts := make([]string, len(got))
		for i, n := range got {
			parts[i] = strconv.Itoa(n)
		}
		if strings.Join(parts, ",") != tc.want {
			t.Fatalf("(%d,%d) = %v, want %q", tc.prev, tc.next, got, tc.want)
		}
	}
}

func TestManRocketLevelPlayMilestonesOnce(t *testing.T) {
	s, ts := newIngestTestServer(t, testManRocketKey)
	base := ts.URL
	uploadMRLevel(t, base, "pad", "Pad", "owner-a")

	status, _, raw := playMR(t, base, "pad", "complete", "pilot-1", "sess-c")
	if status != http.StatusOK {
		t.Fatalf("complete: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad")

	status, _, raw = playMR(t, base, "pad", "start", "pilot-1", "sess-1")
	if status != http.StatusOK {
		t.Fatalf("first start: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1)
	assertMilestoneNote(t, s, "pad", 1, "owner-a")

	status, _, raw = playMR(t, base, "pad", "start", "pilot-2", "sess-1")
	if status != http.StatusOK {
		t.Fatalf("retry: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1)

	status, _, raw = playMR(t, base, "pad", "start", "pilot-1", "sess-2")
	if status != http.StatusOK {
		t.Fatalf("second start: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1)

	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=9 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, body, raw := playMR(t, base, "pad", "start", "pilot-1", "sess-10")
	if status != http.StatusOK || body["play_count"] != float64(10) {
		t.Fatalf("tenth: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1, 10)
	assertMilestoneNote(t, s, "pad", 10, "owner-a")

	status, _, raw = playMR(t, base, "pad", "start", "pilot-1", "sess-11")
	if status != http.StatusOK {
		t.Fatalf("eleventh: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1, 10)

	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=99 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, body, raw = playMR(t, base, "pad", "start", "pilot-1", "sess-100")
	if status != http.StatusOK || body["play_count"] != float64(100) {
		t.Fatalf("hundredth: %d %s", status, raw)
	}
	if _, err := s.store.db.Exec(`UPDATE manrocket_levels SET play_count=499 WHERE id='pad'`); err != nil {
		t.Fatal(err)
	}
	status, body, raw = playMR(t, base, "pad", "start", "pilot-1", "sess-500")
	if status != http.StatusOK || body["play_count"] != float64(500) {
		t.Fatalf("five hundredth: %d %s", status, raw)
	}
	assertMilestones(t, s, "pad", 1, 10, 100, 500)

	// A second level does not reuse the first level's milestone rows.
	uploadMRLevel(t, base, "other", "Other", "owner-b")
	status, _, raw = playMR(t, base, "other", "start", "pilot-1", "sess-1")
	if status != http.StatusOK {
		t.Fatalf("other: %d %s", status, raw)
	}
	assertMilestones(t, s, "other", 1)
	assertMilestoneNote(t, s, "other", 1, "owner-b")
	assertMilestones(t, s, "pad", 1, 10, 100, 500)
}

func assertMilestones(t *testing.T, s *server, levelID string, want ...int) {
	t.Helper()
	rows, err := s.store.db.Query(
		`SELECT threshold FROM manrocket_level_milestones WHERE level_id=? ORDER BY threshold`, levelID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var threshold int
		if err := rows.Scan(&threshold); err != nil {
			t.Fatal(err)
		}
		got = append(got, threshold)
	}
	if len(got) != len(want) {
		t.Fatalf("%s milestones = %v, want %v", levelID, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s milestones = %v, want %v", levelID, got, want)
		}
	}
}

func assertMilestoneNote(t *testing.T, s *server, levelID string, threshold int, owner string) {
	t.Helper()
	var install, kind string
	var n int
	if err := s.store.db.QueryRow(
		`SELECT install_id, kind, COUNT(1) FROM manrocket_level_notifications
		 WHERE level_id=? AND threshold=?`, levelID, threshold,
	).Scan(&install, &kind, &n); err != nil {
		t.Fatal(err)
	}
	if install != owner || kind != mrPlayMilestoneKind || n != 1 {
		t.Fatalf("note %s/%d = %s %s n=%d", levelID, threshold, install, kind, n)
	}
}

func TestManRocketCourseBoardUnaffectedByPlays(t *testing.T) {
	_, ts := newIngestTestServer(t, testManRocketKey)
	base := ts.URL
	uploadMRLevel(t, base, "course-1", "Pad", "owner-a")
	status, _, _ := playMR(t, base, "course-1", "start", "pilot-1", "sess-1")
	if status != http.StatusOK {
		t.Fatalf("play: %d", status)
	}

	status, raw := postJSON(t, base+"/v1/leaderboard", "", map[string]any{
		"game": "manrocket", "mode": "course", "course": "missing",
		"time_ms": 5000, "name": "Ada", "install_id": "inst-course",
	})
	if status != http.StatusNotFound || !strings.Contains(string(raw), "unknown course") {
		t.Fatalf("unknown course: %d %s", status, raw)
	}
	status, raw = postJSON(t, base+"/v1/leaderboard", "", map[string]any{
		"game": "manrocket", "mode": "course", "course": "course-1",
		"time_ms": 5000, "name": "Ada", "install_id": "inst-course",
	})
	if status != http.StatusOK {
		t.Fatalf("course submit: %d %s", status, raw)
	}
	status, board := getMR(t, base+"/v1/leaderboard/manrocket?mode=course&course=course-1")
	if status != http.StatusOK {
		t.Fatalf("course board: %d %#v", status, board)
	}
	entries, ok := board["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("entries: %#v", board["entries"])
	}
}
