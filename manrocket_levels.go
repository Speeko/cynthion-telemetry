package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type mrLevelPayload map[string]any

func (s *server) manrocketLevels(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/manrocket/levels")
	path = strings.Trim(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		s.mrList(w, r)
	case path == "" && r.Method == http.MethodPost:
		s.mrUpload(w, r)
	case strings.HasSuffix(path, "/vote") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(path, "/vote")
		id = strings.Trim(id, "/")
		s.mrVote(w, r, id)
	case path != "" && r.Method == http.MethodGet:
		s.mrGet(w, r, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) mrUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var payload mrLevelPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	id, _ := payload["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 80 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	title, _ := payload["title"].(string)
	title = cleanMRName(title, 48)
	if title == "" {
		title = "Untitled"
	}
	author, _ := payload["author"].(string)
	author = cleanMRName(author, 16)
	if author == "" {
		author = "anon"
	}
	install, _ := payload["install_id"].(string)
	install = strings.TrimSpace(install)
	if install == "" || len(install) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	now := time.Now().UnixMilli()
	_, err = s.store.db.Exec(
		`INSERT INTO manrocket_levels (id, received_at, updated_at, title, author, install_id, votes, payload)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   updated_at=excluded.updated_at,
		   title=excluded.title,
		   author=excluded.author,
		   payload=excluded.payload
		 WHERE manrocket_levels.install_id = excluded.install_id`,
		id, now, now, title, author, install, string(raw),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

func (s *server) mrList(w http.ResponseWriter, r *http.Request) {
	sort := strings.ToLower(r.URL.Query().Get("sort"))
	if sort == "" {
		sort = "top"
	}
	limit := 40
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	order := "votes DESC, received_at DESC"
	if sort == "new" {
		order = "received_at DESC"
	}
	q := `SELECT id, title, author, votes, payload, received_at FROM manrocket_levels ORDER BY ` + order + ` LIMIT ?`
	rows, err := s.store.db.Query(q, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, author, payload string
		var votes int
		var received int64
		if err := rows.Scan(&id, &title, &author, &votes, &payload, &received); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		var level any
		_ = json.Unmarshal([]byte(payload), &level)
		out = append(out, map[string]any{
			"id": id, "title": title, "author": author, "votes": votes,
			"received_at": received, "level": level,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "levels": out})
}

func (s *server) mrGet(w http.ResponseWriter, r *http.Request, id string) {
	var payload string
	var votes int
	err := s.store.db.QueryRow(
		`SELECT payload, votes FROM manrocket_levels WHERE id=?`, id,
	).Scan(&payload, &votes)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var level any
	_ = json.Unmarshal([]byte(payload), &level)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "votes": votes, "level": level})
}

func (s *server) mrVote(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var in struct {
		InstallID string `json:"install_id"`
		Value     int    `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	in.InstallID = strings.TrimSpace(in.InstallID)
	if in.InstallID == "" || len(in.InstallID) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	if in.Value != 1 && in.Value != -1 {
		http.Error(w, "bad value", http.StatusBadRequest)
		return
	}
	tx, err := s.store.db.Begin()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM manrocket_levels WHERE id=?`, id).Scan(&exists); err != nil || exists == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var prev sql.NullInt64
	_ = tx.QueryRow(`SELECT value FROM manrocket_level_votes WHERE level_id=? AND install_id=?`, id, in.InstallID).Scan(&prev)
	delta := in.Value
	if prev.Valid {
		delta = in.Value - int(prev.Int64)
	}
	if _, err := tx.Exec(
		`INSERT INTO manrocket_level_votes (level_id, install_id, value) VALUES (?,?,?)
		 ON CONFLICT(level_id, install_id) DO UPDATE SET value=excluded.value`,
		id, in.InstallID, in.Value,
	); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(`UPDATE manrocket_levels SET votes = votes + ? WHERE id=?`, delta, id); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var votes int
	_ = s.store.db.QueryRow(`SELECT votes FROM manrocket_levels WHERE id=?`, id).Scan(&votes)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "votes": votes})
}

func cleanMRName(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
