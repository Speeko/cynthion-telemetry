// ManRocket analytics ingest. Same request shape as Cynthion's
// POST /v1/events, /v1/crash, and /v1/bugreport, but a different API key
// and different tables/files. These handlers never write the Cynthion
// events, crashes, or bugreports tables (or data/bugreports/).
//
// Auth: X-API-Key against MANROCKET_INGEST_API_KEY. Missing, short (<24),
// or mismatched keys return 401. INGEST_API_KEY is not accepted here.
// If the env var is unset the process still starts; only these three
// routes refuse traffic, so Cynthion ingest keeps working.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const manrocketIngestSchema = `
CREATE TABLE IF NOT EXISTS manrocket_events (
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
CREATE INDEX IF NOT EXISTS idx_mr_events_install ON manrocket_events(install_id);
CREATE INDEX IF NOT EXISTS idx_mr_events_received ON manrocket_events(received_at);
CREATE INDEX IF NOT EXISTS idx_mr_events_type ON manrocket_events(event_type);
CREATE INDEX IF NOT EXISTS idx_mr_events_steam ON manrocket_events(steam_id);

CREATE TABLE IF NOT EXISTS manrocket_crashes (
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
CREATE INDEX IF NOT EXISTS idx_mr_crashes_install ON manrocket_crashes(install_id);
CREATE INDEX IF NOT EXISTS idx_mr_crashes_received ON manrocket_crashes(received_at);

CREATE TABLE IF NOT EXISTS manrocket_bugreports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    install_id TEXT NOT NULL,
    session_id TEXT,
    app_version TEXT,
    os TEXT,
    gpu TEXT,
    category TEXT,
    severity TEXT,
    description TEXT,
    expected_behavior TEXT,
    archive_name TEXT,
    archive_size INTEGER
);
CREATE INDEX IF NOT EXISTS idx_mr_bugreports_install ON manrocket_bugreports(install_id);
CREATE INDEX IF NOT EXISTS idx_mr_bugreports_received ON manrocket_bugreports(received_at);
`

func (s *Store) ensureManRocketIngestSchema() error {
	_, err := s.db.Exec(manrocketIngestSchema)
	return err
}

func (s *Store) insertManRocketEvents(ctx context.Context, b EventsBatch, receivedAt int64) (int, error) {
	if len(b.Events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO manrocket_events
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

func (s *Store) insertManRocketCrash(ctx context.Context, c Crash, receivedAt int64) (int64, error) {
	payload := string(c.Payload)
	if payload == "" {
		payload = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO manrocket_crashes
        (received_at, install_id, session_id, app_version, os, gpu, error_summary, boot_log, player_log, payload)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receivedAt, c.InstallID, c.SessionID, c.AppVersion, c.OS, c.GPU,
		c.ErrorSummary, c.BootLog, c.PlayerLog, payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) insertManRocketBugReport(ctx context.Context, b BugReport, receivedAt int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO manrocket_bugreports
        (received_at, install_id, session_id, app_version, os, gpu, category, severity, description, expected_behavior, archive_name, archive_size)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receivedAt, b.InstallID, b.SessionID, b.AppVersion, b.OS, b.GPU,
		b.Category, b.Severity, b.Description, b.ExpectedBehavior, b.ArchiveName, b.ArchiveSize)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// withManRocketAuth requires X-API-Key to match MANROCKET_INGEST_API_KEY.
// A missing or short key rejects every caller so an unset env var cannot
// accidentally leave ingest open, and so it cannot accept INGEST_API_KEY.
func (s *server) withManRocketAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.ManRocketAPIKey
		got := r.Header.Get("X-API-Key")
		if len(key) < 24 || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) ingestManRocketEvents(w http.ResponseWriter, r *http.Request) {
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
	n, err := s.store.insertManRocketEvents(r.Context(), b, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert manrocket events failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": n})
}

func (s *server) ingestManRocketCrash(w http.ResponseWriter, r *http.Request) {
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
	id, err := s.store.insertManRocketCrash(r.Context(), c, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert manrocket crash failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// ingestManRocketBugReport is the multipart sibling of ingestBugReport.
// Zips land in manrocketBugDir (data/manrocket_bugreports/), metadata in
// manrocket_bugreports. The Cynthion bugreports table and data/bugreports/
// directory are not touched.
func (s *server) ingestManRocketBugReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBugReportBytes)
	if err := r.ParseMultipartForm(bugReportMemBuffer); err != nil {
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

	installID := r.FormValue("install_id")
	if installID == "" {
		http.Error(w, "install_id required", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("archive")
	if err != nil {
		http.Error(w, "archive file required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	receivedAt := time.Now().UnixMilli()
	short := installID
	if len(short) > 8 {
		short = short[:8]
	}
	archiveName := fmt.Sprintf("%d_%s.zip", receivedAt, sanitizeName(short))
	dstPath := filepath.Join(s.manrocketBugDir, archiveName)

	out, err := os.Create(dstPath)
	if err != nil {
		log.Printf("manrocket bugreport create file failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	size, err := io.Copy(out, file)
	closeErr := out.Close()
	if err != nil || closeErr != nil {
		os.Remove(dstPath)
		log.Printf("manrocket bugreport write failed: copy=%v close=%v", err, closeErr)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_ = header // header.Filename is client-supplied; we generate our own name

	id, err := s.store.insertManRocketBugReport(r.Context(), BugReport{
		InstallID:        installID,
		SessionID:        r.FormValue("session_id"),
		AppVersion:       r.FormValue("app_version"),
		OS:               r.FormValue("os"),
		GPU:              r.FormValue("gpu"),
		Category:         r.FormValue("category"),
		Severity:         r.FormValue("severity"),
		Description:      r.FormValue("description"),
		ExpectedBehavior: r.FormValue("expected_behavior"),
		ArchiveName:      archiveName,
		ArchiveSize:      size,
	}, receivedAt)
	if err != nil {
		os.Remove(dstPath)
		log.Printf("insert manrocket bugreport failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "archive": archiveName})
}
