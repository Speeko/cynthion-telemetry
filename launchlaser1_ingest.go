// Launch Laser 1 ingest under Runed Poodle telemetry.
//
// Game-scoped at POST /v1/launchlaser1/events and /v1/launchlaser1/crash
// only (no bugreport, no read or admin routes). The slug is launchlaser1
// because Launch Laser 2 owns "launchlaser" on the leaderboard. Request
// shapes, validation and rate limiting match ManRocket ingest. Storage is
// launchlaser1_events and launchlaser1_crashes only.
//
// Auth: X-API-Key against LAUNCHLASER1_INGEST_API_KEY. Missing, short
// (<24) or mismatched keys return 401. The key ships in the public client,
// so it can only insert rows through these two routes. Other games' keys
// are not accepted here, and this key opens nothing else. If the env var
// is unset the process still starts; only these routes refuse traffic.
// Served on api.cynthiongame.com and api.runedpoodle.com (same Caddy block).
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

const launchlaser1IngestSchema = `
CREATE TABLE IF NOT EXISTS launchlaser1_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    client_ts INTEGER,
    install_id TEXT NOT NULL,
    session_id TEXT,
    app_version TEXT,
    os TEXT,
    gpu TEXT,
    steam_id TEXT,
    persona_name TEXT,
    country TEXT,
    language TEXT,
    event_type TEXT NOT NULL,
    payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_ll1_events_install ON launchlaser1_events(install_id);
CREATE INDEX IF NOT EXISTS idx_ll1_events_received ON launchlaser1_events(received_at);
CREATE INDEX IF NOT EXISTS idx_ll1_events_type ON launchlaser1_events(event_type);

CREATE TABLE IF NOT EXISTS launchlaser1_crashes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    install_id TEXT NOT NULL,
    session_id TEXT,
    app_version TEXT,
    os TEXT,
    gpu TEXT,
    error_summary TEXT,
    boot_log TEXT,
    player_log TEXT,
    payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_ll1_crashes_install ON launchlaser1_crashes(install_id);
CREATE INDEX IF NOT EXISTS idx_ll1_crashes_received ON launchlaser1_crashes(received_at);
`

func (s *Store) ensureLaunchLaser1IngestSchema() error {
	_, err := s.db.Exec(launchlaser1IngestSchema)
	return err
}

func (s *Store) insertLaunchLaser1Events(ctx context.Context, b EventsBatch, receivedAt int64) (int, error) {
	if len(b.Events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO launchlaser1_events
        (received_at, client_ts, install_id, session_id, app_version, os, gpu, steam_id, persona_name, country, language, event_type, payload)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, e := range b.Events {
		payload := string(e.Payload)
		if payload == "" {
			payload = "{}"
		}
		if _, err := stmt.ExecContext(ctx,
			receivedAt, e.ClientTS, b.InstallID, b.SessionID, b.AppVersion, b.OS, b.GPU,
			b.SteamID, b.PersonaName, b.Country, b.Language,
			e.EventType, payload); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

func (s *Store) insertLaunchLaser1Crash(ctx context.Context, c Crash, receivedAt int64) (int64, error) {
	payload := string(c.Payload)
	if payload == "" {
		payload = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO launchlaser1_crashes
        (received_at, install_id, session_id, app_version, os, gpu, error_summary, boot_log, player_log, payload)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receivedAt, c.InstallID, c.SessionID, c.AppVersion, c.OS, c.GPU,
		c.ErrorSummary, c.BootLog, c.PlayerLog, payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// withLaunchLaser1Auth requires X-API-Key to match one of the comma-separated
// keys in LAUNCHLASER1_INGEST_API_KEY.
func (s *server) withLaunchLaser1Auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !ingestKeyMatches(s.cfg.LaunchLaser1APIKey, r.Header.Get("X-API-Key")) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) ingestLaunchLaser1Events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var b EventsBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.InstallID == "" {
		http.Error(w, "install_id required", http.StatusBadRequest)
		return
	}
	if len(b.Events) > maxEventsPerBatch {
		http.Error(w, "too many events in batch", http.StatusBadRequest)
		return
	}
	if err := validateEvents(b.Events); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.allowDaily(w, r, "launchlaser1", "events", b.InstallID, len(b.Events)) {
		return
	}
	n, err := s.store.insertLaunchLaser1Events(r.Context(), b, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert launchlaser1 events failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": n})
}

func (s *server) ingestLaunchLaser1Crash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var c Crash
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if c.InstallID == "" {
		http.Error(w, "install_id required", http.StatusBadRequest)
		return
	}
	if !s.allowDaily(w, r, "launchlaser1", "crashes", c.InstallID, 1) {
		return
	}
	id, err := s.store.insertLaunchLaser1Crash(r.Context(), c, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert launchlaser1 crash failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}
