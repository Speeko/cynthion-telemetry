package main

// Ingest hardening for the game-scoped routes (/v1/manrocket/*, /v1/launchlaser1/*):
//   - event_type must be 1-64 chars of [a-z0-9_.:-]; each event payload <= 8 KB (400 otherwise).
//   - daily caps per install_id and per IP, counted in memory per UTC day (429 + JSON error).
//   - ingest keys accept a comma-separated list (trimmed, each >= 24 chars, constant-time compare).

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxEventTypeLen      = 64
	maxEventPayloadBytes = 8 * 1024
	minIngestKeyLen      = 24
)

var eventTypeRe = regexp.MustCompile(`^[a-z0-9_.:-]+$`)

// validateEvents returns a client-facing error for the first bad event, or nil.
func validateEvents(evs []Event) error {
	for i, e := range evs {
		if len(e.EventType) == 0 || len(e.EventType) > maxEventTypeLen || !eventTypeRe.MatchString(e.EventType) {
			return fmt.Errorf("event %d: event_type must be 1-%d chars of [a-z0-9_.:-]", i, maxEventTypeLen)
		}
		if len(e.Payload) > maxEventPayloadBytes {
			return fmt.Errorf("event %d: payload exceeds %d bytes", i, maxEventPayloadBytes)
		}
	}
	return nil
}

// Daily caps. A heavy ManRocket player sends ~1,500 events/day, so the
// install cap leaves ~6x headroom. Per-IP caps cover shared NAT households.
type dailyCap struct{ install, ip int }

var ingestDailyCaps = map[string]dailyCap{
	"events":     {install: 10000, ip: 50000},
	"crashes":    {install: 200, ip: 1000},
	"bugreports": {install: 20, ip: 50},
}

type dailyCounter struct {
	mu     sync.Mutex
	day    string
	counts map[string]int
}

// take adds n to the install and IP buckets for (game, kind) if both stay
// within the cap; otherwise it changes nothing and returns false.
func (d *dailyCounter) take(now time.Time, game, kind, installID, ip string, n int) bool {
	c, ok := ingestDailyCaps[kind]
	if !ok {
		return true
	}
	day := now.UTC().Format("2006-01-02")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.day != day || d.counts == nil {
		d.day = day
		d.counts = make(map[string]int)
	}
	ki := game + "|" + kind + "|i|" + installID
	kp := game + "|" + kind + "|p|" + ip
	if d.counts[ki]+n > c.install || d.counts[kp]+n > c.ip {
		return false
	}
	d.counts[ki] += n
	d.counts[kp] += n
	return true
}

func (s *server) daily() *dailyCounter {
	s.dailyOnce.Do(func() { s.dailyCaps = &dailyCounter{} })
	return s.dailyCaps
}

// allowDaily writes a 429 JSON response and returns false when over the cap.
func (s *server) allowDaily(w http.ResponseWriter, r *http.Request, game, kind, installID string, n int) bool {
	if s.daily().take(time.Now(), game, kind, installID, clientIP(r), n) {
		return true
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "reason": "daily " + kind + " limit reached"})
	return false
}

// parseIngestKeys splits a comma-separated key list, trims entries, and
// drops empties and keys shorter than minIngestKeyLen.
func parseIngestKeys(raw string) []string {
	var out []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if len(k) >= minIngestKeyLen {
			out = append(out, k)
		}
	}
	return out
}

// ingestKeyMatches compares got against every configured key in constant
// time per key, without short-circuiting on a match.
func ingestKeyMatches(raw, got string) bool {
	match := 0
	for _, k := range parseIngestKeys(raw) {
		match |= subtle.ConstantTimeCompare([]byte(got), []byte(k))
	}
	return got != "" && match == 1
}
