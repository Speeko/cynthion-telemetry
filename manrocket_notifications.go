package main

// ManRocket notification inbox. Public (no analytics API key), keyed by
// install_id, same trust model as level votes.
//
//	GET  /v1/manrocket/notifications?install_id=[&limit=][&offset=][&unread=1]
//	POST /v1/manrocket/notifications/read   {install_id, ids[]|all:true}
//
// Rows live in manrocket_level_notifications, the table the play route
// already fills with kind=play_milestone. This file adds inbox columns
// (actor, comment, read_at) and writes comment, reply, and vote rows into
// the same table. Play milestone uniqueness is unchanged.

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
	notifPlayMilestone  = mrPlayMilestoneKind

	notifListDefault = 40
	notifListMax     = 100
	notifReadMaxIDs  = 200
)

const manrocketCommentSchema = `
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
`

func (s *Store) ensureManRocketSocialSchema() error {
	if _, err := s.db.Exec(manrocketCommentSchema); err != nil {
		return err
	}
	// Play schema creates manrocket_level_notifications. Extend it; do not
	// replace the table the play route inserts into.
	if err := s.ensureNotificationInboxColumns(); err != nil {
		return err
	}
	if err := s.relaxPlayNotificationUniqueness(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_mr_notif_play_once
		ON manrocket_level_notifications(level_id, kind, threshold)
		WHERE kind = 'play_milestone'`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_mr_notif_vote_once
		ON manrocket_level_notifications(level_id, install_id, actor_install_id)
		WHERE kind = 'vote_on_your_level'`); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_mr_notif_inbox
		ON manrocket_level_notifications(install_id, created_at DESC, id DESC)`)
	return err
}

func (st *Store) ensureNotificationInboxColumns() error {
	alters := []string{
		`ALTER TABLE manrocket_level_notifications ADD COLUMN actor_install_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE manrocket_level_notifications ADD COLUMN comment_id INTEGER`,
		`ALTER TABLE manrocket_level_notifications ADD COLUMN read_at INTEGER`,
	}
	for _, q := range alters {
		if _, err := st.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// relaxPlayNotificationUniqueness drops the table-level UNIQUE(level_id, kind, threshold)
// copied from the play stub. That constraint also blocked a second comment or vote
// on the same level. Play rows stay unique via idx_mr_notif_play_once.
func (st *Store) relaxPlayNotificationUniqueness() error {
	var ddl string
	if err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='manrocket_level_notifications'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(ddl), "UNIQUE") {
		return nil
	}
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`CREATE TABLE manrocket_level_notifications_next (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level_id TEXT NOT NULL,
		install_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		threshold INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		actor_install_id TEXT NOT NULL DEFAULT '',
		comment_id INTEGER,
		read_at INTEGER
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO manrocket_level_notifications_next
		(id, level_id, install_id, kind, threshold, created_at, actor_install_id, comment_id, read_at)
		SELECT id, level_id, install_id, kind, threshold, created_at,
		       COALESCE(actor_install_id, ''), comment_id, read_at
		FROM manrocket_level_notifications`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE manrocket_level_notifications`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE manrocket_level_notifications_next RENAME TO manrocket_level_notifications`); err != nil {
		return err
	}
	return tx.Commit()
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
		`SELECT id, kind, actor_install_id, level_id, comment_id, threshold, created_at, read_at
		 FROM manrocket_level_notifications
		 WHERE install_id=? AND (read_at IS NULL OR ? = 0)
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
		var id, threshold, created int64
		var kind, actor, levelID string
		var commentID, readAt sql.NullInt64
		if err := rows.Scan(&id, &kind, &actor, &levelID, &commentID, &threshold, &created, &readAt); err != nil {
			log.Printf("scan notification failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		out = append(out, notificationJSON(id, kind, actor, levelID, commentID, threshold, created, readAt))
	}
	if err := rows.Err(); err != nil {
		log.Printf("list notifications failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notifications": out})
}

func notificationJSON(id int64, kind, actor, levelID string, commentID sql.NullInt64, threshold, created int64, readAt sql.NullInt64) map[string]any {
	var actorV, comment, mile, read any
	if actor != "" {
		actorV = actor
	}
	if commentID.Valid {
		comment = commentID.Int64
	}
	if kind == mrPlayMilestoneKind {
		mile = threshold
	}
	if readAt.Valid {
		read = readAt.Int64
	}
	return map[string]any{
		"id":               id,
		"type":             kind,
		"kind":             kind,
		"actor_install_id": actorV,
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
			`UPDATE manrocket_level_notifications SET read_at=? WHERE install_id=? AND read_at IS NULL`,
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
				`UPDATE manrocket_level_notifications SET read_at=?
				 WHERE id=? AND install_id=? AND read_at IS NULL`,
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

// insertManRocketNotification writes one inbox row on tx. threshold is 0 for
// non-milestone kinds; play_milestone rows are inserted by recordPlayMilestones.
func insertManRocketNotification(tx *sql.Tx, recipient, kind, actor, levelID string, commentID sql.NullInt64, now int64) error {
	if recipient == "" {
		return nil
	}
	_, err := tx.Exec(
		`INSERT INTO manrocket_level_notifications
		 (level_id, install_id, kind, threshold, created_at, actor_install_id, comment_id)
		 VALUES (?, ?, ?, 0, ?, ?, ?)`,
		levelID, recipient, kind, now, actor, commentID,
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
		`INSERT INTO manrocket_level_notifications
		 (level_id, install_id, kind, threshold, created_at, actor_install_id)
		 VALUES (?, ?, ?, 0, ?, ?)
		 ON CONFLICT(level_id, install_id, actor_install_id) WHERE kind = 'vote_on_your_level' DO NOTHING`,
		levelID, owner, notifVoteOnLevel, time.Now().UnixMilli(), actor,
	); err != nil {
		log.Printf("vote notification failed: %v", err)
	}
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
