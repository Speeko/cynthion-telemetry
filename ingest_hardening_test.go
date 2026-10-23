package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

var currentClientEventNames = []string{
	"session_start", "heartbeat", "session_end", "level_start", "level_complete",
	"level_fail", "level_skip", "play_start", "play_complete", "maker_upload", "maker_vote",
}

func batchOf(types ...string) EventsBatch {
	b := sampleBatch(len(types))
	for i, t := range types {
		b.Events[i].EventType = t
	}
	return b
}

func TestCurrentClientEventNamesAccepted(t *testing.T) {
	s, url := newLL1TestServer(t, testLaunchLaser1Key)
	for _, p := range []struct{ path, key string }{
		{"/v1/manrocket/events", testManRocketKey},
		{"/v1/launchlaser1/events", testLaunchLaser1Key},
	} {
		status, raw := postJSON(t, url+p.path, p.key, batchOf(currentClientEventNames...))
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", p.path, status, raw)
		}
	}
	if n := ll1Count(t, s, "manrocket_events"); n != len(currentClientEventNames) {
		t.Fatalf("manrocket_events=%d", n)
	}
}

func TestBadEventRejected(t *testing.T) {
	_, url := newLL1TestServer(t, testLaunchLaser1Key)
	big := sampleBatch(1)
	big.Events[0].Payload = json.RawMessage(`{"x":"` + strings.Repeat("a", maxEventPayloadBytes) + `"}`)
	cases := map[string]EventsBatch{
		"empty":     batchOf(""),
		"upper":     batchOf("Session_Start"),
		"space":     batchOf("session start"),
		"too long":  batchOf(strings.Repeat("a", 65)),
		"mixed bad": batchOf("heartbeat", "bad/type"),
		"oversize":  big,
	}
	for name, b := range cases {
		for _, p := range []struct{ path, key string }{
			{"/v1/manrocket/events", testManRocketKey},
			{"/v1/launchlaser1/events", testLaunchLaser1Key},
		} {
			if status, raw := postJSON(t, url+p.path, p.key, b); status != http.StatusBadRequest {
				t.Fatalf("%s %s status=%d body=%s", name, p.path, status, raw)
			}
		}
	}
	ok := batchOf(strings.Repeat("a", 64), "a.b:c-d_e9")
	if status, raw := postJSON(t, url+"/v1/manrocket/events", testManRocketKey, ok); status != http.StatusOK {
		t.Fatalf("edge names status=%d body=%s", status, raw)
	}
}

func TestDailyCounterCaps(t *testing.T) {
	d := &dailyCounter{}
	now := time.Date(2026, 10, 23, 23, 0, 0, 0, time.UTC)
	if !d.take(now, "manrocket", "events", "i1", "1.1.1.1", 9999) || !d.take(now, "manrocket", "events", "i1", "1.1.1.1", 1) {
		t.Fatal("under cap rejected")
	}
	if d.take(now, "manrocket", "events", "i1", "1.1.1.1", 1) {
		t.Fatal("install cap not enforced")
	}
	if !d.take(now, "launchlaser1", "events", "i1", "1.1.1.1", 1) {
		t.Fatal("games should not share buckets")
	}
	for i := 0; i < 4; i++ {
		if !d.take(now, "manrocket", "events", "x"+string(rune('a'+i)), "2.2.2.2", 10000) {
			t.Fatal("ip under cap rejected")
		}
	}
	if !d.take(now, "manrocket", "events", "xe", "2.2.2.2", 10000) || d.take(now, "manrocket", "events", "xf", "2.2.2.2", 1) {
		t.Fatal("ip cap 50000 not enforced")
	}
	if !d.take(now.Add(2*time.Hour), "manrocket", "events", "i1", "1.1.1.1", 1) {
		t.Fatal("cap did not reset at UTC midnight")
	}
}

func TestDailyCrashCap429(t *testing.T) {
	_, url := newLL1TestServer(t, testLaunchLaser1Key)
	c := Crash{InstallID: "crashy", ErrorSummary: "boom"}
	for i := 0; i < 200; i++ {
		if status, raw := postJSON(t, url+"/v1/manrocket/crash", testManRocketKey, c); status != http.StatusOK {
			t.Fatalf("crash %d status=%d body=%s", i, status, raw)
		}
	}
	status, raw := postJSON(t, url+"/v1/manrocket/crash", testManRocketKey, c)
	var resp struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if status != http.StatusTooManyRequests || json.Unmarshal(raw, &resp) != nil || resp.OK || resp.Reason == "" {
		t.Fatalf("over cap status=%d body=%s", status, raw)
	}
}

func TestIngestKeyList(t *testing.T) {
	second := "second-manrocket-key-24ch!!"
	_, url := newLL1TestServer(t, " "+testLaunchLaser1Key+" , "+second+",short,")
	for _, k := range []string{testLaunchLaser1Key, second} {
		if status, raw := postJSON(t, url+"/v1/launchlaser1/events", k, sampleBatch(1)); status != http.StatusOK {
			t.Fatalf("listed key status=%d body=%s", status, raw)
		}
	}
	for _, k := range []string{"", "short", "nope-not-a-listed-key-at-all"} {
		if status, _ := postJSON(t, url+"/v1/launchlaser1/events", k, sampleBatch(1)); status != http.StatusUnauthorized {
			t.Fatalf("key %q status=%d", k, status)
		}
	}
	if got := parseIngestKeys(" a , ,"); len(got) != 0 {
		t.Fatalf("short keys kept: %d", len(got))
	}
	if got := parseIngestKeys(testManRocketKey); len(got) != 1 {
		t.Fatal("single key form broken")
	}
}
