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
	case strings.HasSuffix(path, "/play") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(path, "/play")
		id = strings.Trim(id, "/")
		s.mrPlay(w, r, id)
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
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 10000 {
			offset = n
		}
	}
	// sort is a closed set. Anything else stays on the historical default (votes).
	order := "votes DESC, received_at DESC"
	switch sort {
	case "new":
		order = "received_at DESC"
	case "plays":
		order = "play_count DESC, votes DESC, received_at DESC"
	}
	q := `SELECT id, title, author, votes, play_count, completion_count, payload, received_at
		FROM manrocket_levels ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	rows, err := s.store.db.Query(q, limit, offset)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, title, author, payload string
		var votes, playCount, completionCount int
		var received int64
		if err := rows.Scan(&id, &title, &author, &votes, &playCount, &completionCount, &payload, &received); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		var level any
		_ = json.Unmarshal([]byte(payload), &level)
		out = append(out, map[string]any{
			"id": id, "title": title, "author": author, "votes": votes,
			"play_count": playCount, "completion_count": completionCount,
			"received_at": received, "level": level,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "levels": out})
}

func (s *server) mrGet(w http.ResponseWriter, r *http.Request, id string) {
	var payload string
	var votes, playCount, completionCount int
	err := s.store.db.QueryRow(
		`SELECT payload, votes, play_count, completion_count FROM manrocket_levels WHERE id=?`, id,
	).Scan(&payload, &votes, &playCount, &completionCount)
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
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "votes": votes, "play_count": playCount, "completion_count": completionCount, "level": level,
	})
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

// mrPlay records a Maker-level start or completion. Auth matches votes:
// public, CORS + rate limit, install_id in the body, no ingest API key.
// A repeat of the same (level, session, phase) does not increment again.
func (s *server) mrPlay(w http.ResponseWriter, r *http.Request, id string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<14)
	var in struct {
		Phase     string `json:"phase"`
		InstallID string `json:"install_id"`
		SessionID string `json:"session_id"`
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
	in.SessionID = strings.TrimSpace(in.SessionID)
	if in.SessionID == "" || len(in.SessionID) > 64 {
		http.Error(w, "bad session_id", http.StatusBadRequest)
		return
	}
	phase := strings.ToLower(strings.TrimSpace(in.Phase))
	if phase != "start" && phase != "complete" {
		http.Error(w, "bad phase", http.StatusBadRequest)
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
	now := time.Now().UnixMilli()
	res, err := tx.Exec(
		`INSERT INTO manrocket_level_plays (level_id, session_id, phase, install_id, received_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(level_id, session_id, phase) DO NOTHING`,
		id, in.SessionID, phase, in.InstallID, now,
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	counted := n > 0
	if counted {
		col := "play_count"
		if phase == "complete" {
			col = "completion_count"
		}
		if _, err := tx.Exec(`UPDATE manrocket_levels SET `+col+` = `+col+` + 1 WHERE id=?`, id); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		// Milestones follow play_count only. A completion does not cross them.
		if phase == "start" {
			if err := recordPlayMilestones(tx, id, now); err != nil {
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var playCount, completionCount int
	if err := s.store.db.QueryRow(
		`SELECT play_count, completion_count FROM manrocket_levels WHERE id=?`, id,
	).Scan(&playCount, &completionCount); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "play_count": playCount, "completion_count": completionCount, "counted": counted,
	})
}

// ensureManRocketLevelPlaySchema adds denormalized play counters to levels
// that predate them, plus the idempotency table. The plays index is created
// here, after the columns exist — the schema string runs first and must not
// reference play_count on an old database.
func (st *Store) ensureManRocketLevelPlaySchema() error {
	for _, col := range []string{"play_count", "completion_count"} {
		q := `ALTER TABLE manrocket_levels ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`
		if _, err := st.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	if _, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS manrocket_level_plays (
		level_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		phase TEXT NOT NULL,
		install_id TEXT NOT NULL,
		received_at INTEGER NOT NULL,
		PRIMARY KEY (level_id, session_id, phase)
	)`); err != nil {
		return err
	}
	if _, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS manrocket_level_milestones (
		level_id TEXT NOT NULL,
		threshold INTEGER NOT NULL,
		reached_at INTEGER NOT NULL,
		PRIMARY KEY (level_id, threshold)
	)`); err != nil {
		return err
	}
	// Stub for a later inbox / FCM push. Nothing reads or sends these rows yet.
	if _, err := st.db.Exec(`CREATE TABLE IF NOT EXISTS manrocket_level_notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level_id TEXT NOT NULL,
		install_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		threshold INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE (level_id, kind, threshold)
	)`); err != nil {
		return err
	}
	_, err := st.db.Exec(`CREATE INDEX IF NOT EXISTS idx_mr_levels_plays ON manrocket_levels(play_count DESC, votes DESC, received_at DESC)`)
	return err
}

// Play-count milestones. A later inbox and FCM push can consume the stub
// notification rows; this path only persists them once.
var mrPlayMilestoneThresholds = [...]int{1, 10, 100, 500}

const mrPlayMilestoneKind = "play_milestone"

// mrCrossedPlayMilestones returns thresholds in (prev, next]. Play starts
// increment by one, so this is the single threshold equal to the new count.
func mrCrossedPlayMilestones(prev, next int) []int {
	var crossed []int
	for _, threshold := range mrPlayMilestoneThresholds {
		if prev < threshold && next >= threshold {
			crossed = append(crossed, threshold)
		}
	}
	return crossed
}

func recordPlayMilestones(tx *sql.Tx, levelID string, now int64) error {
	var playCount int
	var owner string
	if err := tx.QueryRow(
		`SELECT play_count, install_id FROM manrocket_levels WHERE id=?`, levelID,
	).Scan(&playCount, &owner); err != nil {
		return err
	}
	for _, threshold := range mrCrossedPlayMilestones(playCount-1, playCount) {
		res, err := tx.Exec(
			`INSERT INTO manrocket_level_milestones (level_id, threshold, reached_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(level_id, threshold) DO NOTHING`,
			levelID, threshold, now,
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO manrocket_level_notifications (level_id, install_id, kind, threshold, created_at)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(level_id, kind, threshold) DO NOTHING`,
			levelID, owner, mrPlayMilestoneKind, threshold, now,
		); err != nil {
			return err
		}
	}
	return nil
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
