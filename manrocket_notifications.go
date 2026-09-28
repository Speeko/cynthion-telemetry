package main

// ManRocket notification inbox, plus the play-count ledger a future play
// route should call. Public (no analytics API key), keyed by install_id,
// same trust model as level votes.
//
//	GET  /v1/manrocket/notifications?install_id=[&limit=][&offset=][&unread=1]
//	POST /v1/manrocket/notifications/read   {install_id, ids[]|all:true}
//
// Rows are written when a comment, reply, or new +1 vote lands, and when
// NoteManRocketLevelPlay crosses 1, 10, 100, or 500 plays for the first time.

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	notifCommentOnLevel = "comment_on_your_level"
	notifReplyToYou     = "reply_to_you"
	notifVoteOnLevel    = "vote_on_your_level"
	notifPlayMilestone  = "play_milestone"

	notifListDefault = 40
	notifListMax     = 100
	notifReadMaxIDs  = 200
)

// playMilestones are the play_count thresholds that emit play_milestone once.
var playMilestones = []int{1, 10, 100, 500}

const manrocketSocialSchema = `
CREATE TABLE IF NOT EXISTS manrocket_level_comments (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	level_id TEXT NOT NULL,
	install_id TEXT NOT NULL,
	author TEXT NOT NULL,
	body TEXT NOT NULL,
	reply_to INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	deleted_at INTEGER,
	ip TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mr_comments_level ON manrocket_level_comments(level_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_mr_comments_reply ON manrocket_level_comments(level_id, reply_to, created_at);
CREATE INDEX IF NOT EXISTS idx_mr_comments_install ON manrocket_level_comments(install_id, created_at);
CREATE INDEX IF NOT EXISTS idx_mr_comments_ip ON manrocket_level_comments(ip, created_at);

CREATE TABLE IF NOT EXISTS manrocket_notifications (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	recipient_install_id TEXT NOT NULL,
	type TEXT NOT NULL,
	actor_install_id TEXT NOT NULL,
	level_id TEXT,
	comment_id INTEGER,
	milestone INTEGER,
	created_at INTEGER NOT NULL,
	read_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_mr_notif_inbox ON manrocket_notifications(recipient_install_id, created_at DESC, id DESC);
-- One vote notification per (owner, voter, level). Repeat +1s do not stack.
CREATE UNIQUE INDEX IF NOT EXISTS idx_mr_notif_vote_once
	ON manrocket_notifications(recipient_install_id, actor_install_id, level_id)
	WHERE type = 'vote_on_your_level';

CREATE TABLE IF NOT EXISTS manrocket_level_play_counts (
	level_id TEXT PRIMARY KEY,
	play_count INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS manrocket_level_play_milestones (
	level_id TEXT NOT NULL,
	milestone INTEGER NOT NULL,
	reached_at INTEGER NOT NULL,
	PRIMARY KEY (level_id, milestone)
);
`

func (s *Store) ensureManRocketSocialSchema() error {
	_, err := s.db.Exec(manrocketSocialSchema)
	return err
}

func (s *server) manrocketNotifications(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/manrocket/notifications"), "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.mrListNotifications(w, r)
	case path == "read" && r.Method == http.MethodPost:
		s.mrReadNotifications(w, r)
	case path == "" || path == "read":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *server) mrListNotifications(w http.ResponseWriter, r *http.Request) {
	install, ok := cleanInstallID(r.URL.Query().Get("install_id"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	limit, offset, errReason := socialLimitOffset(r, notifListDefault, notifListMax)
	if errReason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": errReason})
		return
	}
	unreadOnly := queryFlag(r.URL.Query().Get("unread"))
	unreadParam := 0
	if unreadOnly {
		unreadParam = 1
	}
	rows, err := s.store.db.Query(
		`SELECT id, type, actor_install_id, level_id, comment_id, milestone, created_at, read_at
		 FROM manrocket_notifications
		 WHERE recipient_install_id=? AND (read_at IS NULL OR ? = 0)
		 ORDER BY created_at DESC, id DESC
		 LIMIT ? OFFSET ?`,
		install, unreadParam, limit, offset,
	)
	if err != nil {
		log.Printf("list notifications failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, created int64
		var typ, actor string
		var level sql.NullString
		var commentID, milestone, readAt sql.NullInt64
		if err := rows.Scan(&id, &typ, &actor, &level, &commentID, &milestone, &created, &readAt); err != nil {
			log.Printf("scan notification failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		out = append(out, notificationJSON(id, typ, actor, level, commentID, milestone, readAt, created))
	}
	if err := rows.Err(); err != nil {
		log.Printf("list notifications failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notifications": out})
}

func notificationJSON(id int64, typ, actor string, level sql.NullString, commentID, milestone, readAt sql.NullInt64, created int64) map[string]any {
	var levelID, comment, mile, read any
	if level.Valid {
		levelID = level.String
	}
	if commentID.Valid {
		comment = commentID.Int64
	}
	if milestone.Valid {
		mile = milestone.Int64
	}
	if readAt.Valid {
		read = readAt.Int64
	}
	return map[string]any{
		"id":               id,
		"type":             typ,
		"actor_install_id": actor,
		"level_id":         levelID,
		"comment_id":       comment,
		"milestone":        mile,
		"created_at":       created,
		"read_at":          read,
		"read":             readAt.Valid,
	}
}

func (s *server) mrReadNotifications(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var in struct {
		InstallID string  `json:"install_id"`
		IDs       []int64 `json:"ids"`
		All       bool    `json:"all"`
	}
	if err := decodeJSONBody(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad json"})
		return
	}
	install, ok := cleanInstallID(in.InstallID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	if !in.All && len(in.IDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "ids or all required"})
		return
	}
	if len(in.IDs) > notifReadMaxIDs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "too many ids"})
		return
	}
	now := time.Now().UnixMilli()
	var updated int64
	var err error
	if in.All {
		res, execErr := s.store.db.Exec(
			`UPDATE manrocket_notifications SET read_at=? WHERE recipient_install_id=? AND read_at IS NULL`,
			now, install,
		)
		if execErr != nil {
			err = execErr
		} else {
			updated, err = res.RowsAffected()
		}
	} else {
		for _, id := range in.IDs {
			if id <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad id"})
				return
			}
		}
		tx, txErr := s.store.db.Begin()
		if txErr != nil {
			log.Printf("mark notifications read failed: %v", txErr)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		for _, id := range in.IDs {
			res, execErr := tx.Exec(
				`UPDATE manrocket_notifications SET read_at=?
				 WHERE id=? AND recipient_install_id=? AND read_at IS NULL`,
				now, id, install,
			)
			if execErr != nil {
				log.Printf("mark notifications read failed: %v", execErr)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			n, affErr := res.RowsAffected()
			if affErr != nil {
				log.Printf("mark notifications read failed: %v", affErr)
				http.Error(w, "db error", http.StatusInternalServerError)
				return
			}
			updated += n
		}
		if err = tx.Commit(); err != nil {
			log.Printf("mark notifications read failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}
	if err != nil {
		log.Printf("mark notifications read failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updated": updated})
}

// insertManRocketNotification writes one inbox row on tx. commentID and
// milestone are NULL when invalid. The caller decides whether a self-event
// should be skipped; this helper writes whatever it is given.
func insertManRocketNotification(tx *sql.Tx, recipient, typ, actor, levelID string, commentID, milestone sql.NullInt64, now int64) error {
	if recipient == "" {
		return nil
	}
	_, err := tx.Exec(
		`INSERT INTO manrocket_notifications
		 (recipient_install_id, type, actor_install_id, level_id, comment_id, milestone, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		recipient, typ, actor, levelID, commentID, milestone, now,
	)
	return err
}

// notifyVoteOnLevel writes vote_on_your_level the first time actor likes
// a level they do not own. The partial unique index drops repeats. Failures
// are logged; the vote itself has already committed.
func (s *server) notifyVoteOnLevel(levelID, actor string) {
	var owner string
	if err := s.store.db.QueryRow(`SELECT install_id FROM manrocket_levels WHERE id=?`, levelID).Scan(&owner); err != nil {
		return
	}
	if owner == "" || owner == actor {
		return
	}
	if _, err := s.store.db.Exec(
		`INSERT OR IGNORE INTO manrocket_notifications
		 (recipient_install_id, type, actor_install_id, level_id, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		owner, notifVoteOnLevel, actor, levelID, time.Now().UnixMilli(),
	); err != nil {
		log.Printf("vote notification failed: %v", err)
	}
}

// NoteManRocketLevelPlay is the play-route hook. There is no public play
// endpoint yet. Call this once per play: it increments
// manrocket_level_play_counts and, the first time play_count reaches 1, 10,
// 100, or 500, writes one play_milestone notification to the level owner.
// actorInstallID is the player (stored on the row; may equal the owner).
// now is unix milliseconds; <= 0 means time.Now.
// Returns milestones newly crossed. sql.ErrNoRows if the level does not exist.
func (st *Store) NoteManRocketLevelPlay(levelID, actorInstallID string, now int64) ([]int, error) {
	levelID = strings.TrimSpace(levelID)
	if levelID == "" || len(levelID) > 80 {
		return nil, sql.ErrNoRows
	}
	actorInstallID = strings.TrimSpace(actorInstallID)
	if len(actorInstallID) > 64 {
		actorInstallID = actorInstallID[:64]
	}
	if now <= 0 {
		now = time.Now().UnixMilli()
	}
	tx, err := st.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var owner string
	if err := tx.QueryRow(`SELECT install_id FROM manrocket_levels WHERE id=?`, levelID).Scan(&owner); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO manrocket_level_play_counts (level_id, play_count, updated_at) VALUES (?, 1, ?)
		 ON CONFLICT(level_id) DO UPDATE SET play_count = play_count + 1, updated_at = excluded.updated_at`,
		levelID, now,
	); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRow(`SELECT play_count FROM manrocket_level_play_counts WHERE level_id=?`, levelID).Scan(&count); err != nil {
		return nil, err
	}
	var crossed []int
	for _, m := range playMilestones {
		if count < m {
			continue
		}
		res, err := tx.Exec(
			`INSERT INTO manrocket_level_play_milestones (level_id, milestone, reached_at) VALUES (?, ?, ?)
			 ON CONFLICT(level_id, milestone) DO NOTHING`,
			levelID, m, now,
		)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		crossed = append(crossed, m)
		if owner == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO manrocket_notifications
			 (recipient_install_id, type, actor_install_id, level_id, milestone, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			owner, notifPlayMilestone, actorInstallID, levelID, m, now,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return crossed, nil
}

func cleanInstallID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return "", false
	}
	return s, true
}

func socialLimitOffset(r *http.Request, def, max int) (int, int, string) {
	limit := def
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return 0, 0, "bad limit"
		}
		if n > max {
			n = max
		}
		limit = n
	}
	offset := 0
	if v := strings.TrimSpace(r.URL.Query().Get("offset")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10000 {
			return 0, 0, "bad offset"
		}
		offset = n
	}
	return limit, offset, ""
}

func queryFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
