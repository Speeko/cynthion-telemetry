package main

// ManRocket whiteboards: a community pool of player drawings (shown at random on the meeting-room
// whiteboard) plus one Director-owned "default" board that every player sees on first boot.
//
//   POST /v1/manrocket/whiteboards            {png_base64, name, install_id} -> {ok, id}
//   GET  /v1/manrocket/whiteboards/random?limit=N (N<=10) -> {ok, boards:[{id, name, png_base64}]}
//   GET  /v1/manrocket/whiteboards/default[?have=V] -> {ok, version, updated_at, png_base64}
//                                                   (png omitted + unchanged:true when have==version)
//   PUT/POST /v1/manrocket/whiteboards/default  X-Admin-Key + {png_base64} -> {ok, version}

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	wbMaxPNGBytes    = 300 << 10 // decoded PNG cap
	wbMaxBodyBytes   = 512 << 10 // base64 (+33%) + JSON headroom
	wbMaxW, wbMaxH   = 1600, 800
	wbDailyInstall   = 5  // uploads per install_id per 24 h
	wbDailyIP        = 20 // uploads per IP per 24 h (install_id is client-chosen)
	wbRandomMaxLimit = 10
)

const wbSchema = `
CREATE TABLE IF NOT EXISTS manrocket_whiteboards (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at INTEGER NOT NULL,
	name TEXT NOT NULL,
	install_id TEXT NOT NULL,
	ip TEXT NOT NULL,
	hidden INTEGER NOT NULL DEFAULT 0,
	png BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mr_wb_install ON manrocket_whiteboards(install_id, received_at);
CREATE INDEX IF NOT EXISTS idx_mr_wb_ip ON manrocket_whiteboards(ip, received_at);
CREATE TABLE IF NOT EXISTS manrocket_whiteboard_default (
	slot INTEGER PRIMARY KEY CHECK (slot = 1),
	version INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	png BLOB NOT NULL
);
`

func (s *Store) ensureWhiteboardSchema() error {
	_, err := s.db.Exec(wbSchema)
	return err
}

func (s *server) manrocketWhiteboards(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/manrocket/whiteboards"), "/")
	switch {
	case path == "" && r.Method == http.MethodPost:
		s.wbUpload(w, r)
	case path == "random" && r.Method == http.MethodGet:
		s.wbRandom(w, r)
	case path == "default" && r.Method == http.MethodGet:
		s.wbGetDefault(w, r)
	case path == "default" && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		s.wbSetDefault(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// decodeBoardPNG base64-decodes and validates a whiteboard PNG (size cap, real PNG, <= 1600x800).
func decodeBoardPNG(b64 string) ([]byte, string) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) == 0 {
		return nil, "bad png_base64"
	}
	if len(raw) > wbMaxPNGBytes {
		return nil, "png too large"
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "not a png"
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > wbMaxW || cfg.Height > wbMaxH {
		return nil, "bad dimensions (max 1600x800)"
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		return nil, "corrupt png"
	}
	return raw, ""
}

func (s *server) wbUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, wbMaxBodyBytes)
	var in struct {
		PNGBase64 string `json:"png_base64"`
		Name      string `json:"name"`
		InstallID string `json:"install_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	install := strings.TrimSpace(in.InstallID)
	if install == "" || len(install) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	name := cleanMRName(in.Name, 16)
	if name == "" {
		name = "anon"
	}
	raw, msg := decodeBoardPNG(in.PNGBase64)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	ip := ipTag(r)
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var byInstall, byIP int
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_whiteboards WHERE install_id=? AND received_at>?`, install, since).Scan(&byInstall); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_whiteboards WHERE ip=? AND received_at>?`, ip, since).Scan(&byIP); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if byInstall >= wbDailyInstall || byIP >= wbDailyIP {
		http.Error(w, "daily whiteboard limit reached", http.StatusTooManyRequests)
		return
	}
	res, err := s.store.db.Exec(
		`INSERT INTO manrocket_whiteboards (received_at, name, install_id, ip, png) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), name, install, ip, raw,
	)
	if err != nil {
		log.Printf("insert whiteboard failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *server) wbRandom(w http.ResponseWriter, r *http.Request) {
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, wbRandomMaxLimit)
		}
	}
	rows, err := s.store.db.Query(
		`SELECT id, name, png FROM manrocket_whiteboards WHERE hidden=0 ORDER BY RANDOM() LIMIT ?`, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var raw []byte
		if err := rows.Scan(&id, &name, &raw); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		out = append(out, map[string]any{"id": id, "name": name, "png_base64": base64.StdEncoding.EncodeToString(raw)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "boards": out})
}

func (s *server) wbGetDefault(w http.ResponseWriter, r *http.Request) {
	var version, updated int64
	var raw []byte
	err := s.store.db.QueryRow(`SELECT version, updated_at, png FROM manrocket_whiteboard_default WHERE slot=1`).
		Scan(&version, &updated, &raw)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": 0})
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if have := r.URL.Query().Get("have"); have != "" && have == strconv.FormatInt(version, 10) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "updated_at": updated, "unchanged": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": version, "updated_at": updated,
		"png_base64": base64.StdEncoding.EncodeToString(raw),
	})
}

// wbSetDefault replaces the Director's default board. Needs MANROCKET_ADMIN_KEY (env) in X-Admin-Key;
// with the env var unset the slot is read-only.
func (s *server) wbSetDefault(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("MANROCKET_ADMIN_KEY")
	got := r.Header.Get("X-Admin-Key")
	if len(key) < 24 || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, wbMaxBodyBytes)
	var in struct {
		PNGBase64 string `json:"png_base64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	raw, msg := decodeBoardPNG(in.PNGBase64)
	if msg != "" {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()
	if _, err := s.store.db.Exec(
		`INSERT INTO manrocket_whiteboard_default (slot, version, updated_at, png) VALUES (1, 1, ?, ?)
		 ON CONFLICT(slot) DO UPDATE SET version = version + 1, updated_at = excluded.updated_at, png = excluded.png`,
		now, raw,
	); err != nil {
		log.Printf("set default whiteboard failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var version int64
	_ = s.store.db.QueryRow(`SELECT version FROM manrocket_whiteboard_default WHERE slot=1`).Scan(&version)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version})
}

// ipTag is a short one-way hash of the client IP: enough for per-IP abuse caps without storing IPs.
func ipTag(r *http.Request) string {
	sum := sha256.Sum256([]byte("manrocket|" + clientIP(r)))
	return hex.EncodeToString(sum[:8])
}
