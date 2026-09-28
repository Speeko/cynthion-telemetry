package main

// ManRocket level comments. Public (no analytics API key), same install_id
// ownership model as votes.
//
//	GET    /v1/manrocket/levels/<id>/comments[?sort=new|thread][&reply_to=|&parent_id=][&limit=][&offset=]
//	POST   /v1/manrocket/levels/<id>/comments   {install_id, author?, body, reply_to?}
//	PATCH  /v1/manrocket/levels/<id>/comments/<comment_id>   {install_id, body?, author?}
//	DELETE /v1/manrocket/levels/<id>/comments/<comment_id>   {install_id} or ?install_id=
//	POST   /v1/manrocket/levels/<id>/comments/<comment_id>/delete

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	commentMaxBodyRunes = 500
	commentMaxBodyBytes = 16 << 10
	commentDailyInstall = 40
	commentDailyIP      = 120
	commentListDefault  = 40
	commentListMax      = 100
	commentThreadCap    = 500 // live comments assembled into a thread view
)

type mrComment struct {
	ID        int64
	LevelID   string
	InstallID string
	Author    string
	Body      string
	ReplyTo   sql.NullInt64
	CreatedAt int64
	UpdatedAt int64
}

func (c mrComment) jsonMap(replies []any) map[string]any {
	var reply any
	if c.ReplyTo.Valid {
		reply = c.ReplyTo.Int64
	}
	m := map[string]any{
		"id":         c.ID,
		"level_id":   c.LevelID,
		"install_id": c.InstallID,
		"author":     c.Author,
		"body":       c.Body,
		"reply_to":   reply,
		"created_at": c.CreatedAt,
		"updated_at": c.UpdatedAt,
	}
	if replies != nil {
		m["replies"] = replies
	}
	return m
}

// splitLevelComments reports whether path is "<level id>/comments" or
// "<level id>/comments/<rest>". Level ids may themselves contain slashes;
// the "/comments" segment is the boundary, matching the "/vote" suffix.
func isLevelCommentsPath(path string) bool {
	_, _, ok := splitLevelComments(path)
	return ok
}

func splitLevelComments(path string) (levelID, rest string, ok bool) {
	const needle = "/comments"
	i := strings.Index(path, needle)
	if i <= 0 {
		return "", "", false
	}
	after := path[i+len(needle):]
	if after != "" && !strings.HasPrefix(after, "/") {
		return "", "", false
	}
	return path[:i], strings.Trim(after, "/"), true
}

func (s *server) mrComments(w http.ResponseWriter, r *http.Request, path string) {
	levelID, rest, ok := splitLevelComments(path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	levelID = strings.TrimSpace(levelID)
	if levelID == "" || len(levelID) > 80 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad id"})
		return
	}
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.mrListComments(w, r, levelID)
		case http.MethodPost:
			s.mrCreateComment(w, r, levelID)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	parts := strings.Split(rest, "/")
	cid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || cid <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad id"})
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			s.mrPatchComment(w, r, levelID, cid)
		case http.MethodDelete:
			s.mrDeleteComment(w, r, levelID, cid)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost {
		s.mrDeleteComment(w, r, levelID, cid)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (s *server) mrListComments(w http.ResponseWriter, r *http.Request, levelID string) {
	if !s.mrLevelExists(w, levelID) {
		return
	}
	limit, offset, errReason := socialLimitOffset(r, commentListDefault, commentListMax)
	if errReason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": errReason})
		return
	}
	sortMode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	if sortMode == "" {
		sortMode = "new"
	}
	threaded := sortMode == "thread" || sortMode == "threaded"
	if sortMode != "new" && !threaded {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad sort"})
		return
	}
	parent, hasParent, parentOK := commentParentQuery(r)
	if !parentOK {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad reply_to"})
		return
	}
	if threaded {
		s.mrListCommentsThreaded(w, levelID, parent, hasParent, limit, offset)
		return
	}
	q := `SELECT id, level_id, install_id, author, body, reply_to, created_at, updated_at
		FROM manrocket_level_comments
		WHERE level_id=? AND deleted_at IS NULL`
	args := []any{levelID}
	if hasParent {
		q += ` AND reply_to=?`
		args = append(args, parent)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.store.db.Query(q, args...)
	if err != nil {
		log.Printf("list comments failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			log.Printf("scan comment failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		out = append(out, c.jsonMap(nil))
	}
	if err := rows.Err(); err != nil {
		log.Printf("list comments failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sort": "new", "comments": out})
}

func (s *server) mrListCommentsThreaded(w http.ResponseWriter, levelID string, parent int64, hasParent bool, limit, offset int) {
	rows, err := s.store.db.Query(
		`SELECT id, level_id, install_id, author, body, reply_to, created_at, updated_at
		 FROM manrocket_level_comments
		 WHERE level_id=? AND deleted_at IS NULL
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`,
		levelID, commentThreadCap,
	)
	if err != nil {
		log.Printf("list comments failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var all []mrComment
	byID := map[int64]mrComment{}
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			log.Printf("scan comment failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		all = append(all, c)
		byID[c.ID] = c
	}
	if err := rows.Err(); err != nil {
		log.Printf("list comments failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	children := map[int64][]mrComment{}
	var roots []mrComment
	for _, c := range all {
		parentVisible := c.ReplyTo.Valid
		if parentVisible {
			_, parentVisible = byID[c.ReplyTo.Int64]
		}
		if hasParent {
			if c.ReplyTo.Valid && c.ReplyTo.Int64 == parent {
				roots = append(roots, c)
				continue
			}
			if parentVisible {
				children[c.ReplyTo.Int64] = append(children[c.ReplyTo.Int64], c)
			}
			continue
		}
		if parentVisible {
			children[c.ReplyTo.Int64] = append(children[c.ReplyTo.Int64], c)
		} else {
			roots = append(roots, c)
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].CreatedAt == roots[j].CreatedAt {
			return roots[i].ID > roots[j].ID
		}
		return roots[i].CreatedAt > roots[j].CreatedAt
	})
	if offset > len(roots) {
		offset = len(roots)
	}
	end := offset + limit
	if end > len(roots) {
		end = len(roots)
	}
	page := roots[offset:end]
	out := make([]map[string]any, 0, len(page))
	seen := map[int64]bool{}
	for _, c := range page {
		out = append(out, nestComment(c, children, seen, 0))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sort": "thread", "comments": out})
}

func nestComment(c mrComment, children map[int64][]mrComment, seen map[int64]bool, depth int) map[string]any {
	if depth > 8 || seen[c.ID] {
		return c.jsonMap([]any{})
	}
	seen[c.ID] = true
	kids := append([]mrComment(nil), children[c.ID]...)
	sort.Slice(kids, func(i, j int) bool {
		if kids[i].CreatedAt == kids[j].CreatedAt {
			return kids[i].ID < kids[j].ID
		}
		return kids[i].CreatedAt < kids[j].CreatedAt
	})
	nested := make([]any, 0, len(kids))
	for _, k := range kids {
		nested = append(nested, nestComment(k, children, seen, depth+1))
	}
	return c.jsonMap(nested)
}

func (s *server) mrCreateComment(w http.ResponseWriter, r *http.Request, levelID string) {
	r.Body = http.MaxBytesReader(w, r.Body, commentMaxBodyBytes)
	var in struct {
		InstallID string `json:"install_id"`
		Author    string `json:"author"`
		Body      string `json:"body"`
		ReplyTo   *int64 `json:"reply_to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad json"})
		return
	}
	install, ok := cleanInstallID(in.InstallID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	body := fbClean(in.Body, true)
	if body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "empty"})
		return
	}
	if utf8.RuneCountInString(body) > commentMaxBodyRunes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "too long"})
		return
	}
	author := cleanMRName(in.Author, 16)
	if author == "" {
		author = "anon"
	}
	var reply sql.NullInt64
	if in.ReplyTo != nil {
		if *in.ReplyTo <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad reply_to"})
			return
		}
		reply = sql.NullInt64{Int64: *in.ReplyTo, Valid: true}
	}

	tx, err := s.store.db.Begin()
	if err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	var owner string
	err = tx.QueryRow(`SELECT install_id FROM manrocket_levels WHERE id=?`, levelID).Scan(&owner)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "reason": "not found"})
		return
	}
	if err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	var parentOwner string
	if reply.Valid {
		err = tx.QueryRow(
			`SELECT install_id FROM manrocket_level_comments
			 WHERE id=? AND level_id=? AND deleted_at IS NULL`,
			reply.Int64, levelID,
		).Scan(&parentOwner)
		if err == sql.ErrNoRows {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad reply_to"})
			return
		}
		if err != nil {
			log.Printf("create comment failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}

	ip := ipTag(r)
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var byInstall, byIP int
	if err := tx.QueryRow(
		`SELECT COUNT(1) FROM manrocket_level_comments WHERE install_id=? AND created_at>?`,
		install, since,
	).Scan(&byInstall); err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := tx.QueryRow(
		`SELECT COUNT(1) FROM manrocket_level_comments WHERE ip=? AND created_at>?`,
		ip, since,
	).Scan(&byIP); err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if byInstall >= commentDailyInstall || byIP >= commentDailyIP {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "reason": "daily comment limit reached"})
		return
	}

	now := time.Now().UnixMilli()
	res, err := tx.Exec(
		`INSERT INTO manrocket_level_comments
		 (level_id, install_id, author, body, reply_to, created_at, updated_at, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		levelID, install, author, body, reply, now, now, ip,
	)
	if err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	commentRef := sql.NullInt64{Int64: id, Valid: true}
	if owner != install {
		if err := insertManRocketNotification(tx, owner, notifCommentOnLevel, install, levelID, commentRef, now); err != nil {
			log.Printf("create comment failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}
	if reply.Valid && parentOwner != install {
		if err := insertManRocketNotification(tx, parentOwner, notifReplyToYou, install, levelID, commentRef, now); err != nil {
			log.Printf("create comment failed: %v", err)
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("create comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	c := mrComment{
		ID: id, LevelID: levelID, InstallID: install, Author: author, Body: body,
		ReplyTo: reply, CreatedAt: now, UpdatedAt: now,
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "comment": c.jsonMap(nil)})
}

func (s *server) mrPatchComment(w http.ResponseWriter, r *http.Request, levelID string, id int64) {
	r.Body = http.MaxBytesReader(w, r.Body, commentMaxBodyBytes)
	var in struct {
		InstallID string  `json:"install_id"`
		Author    *string `json:"author"`
		Body      *string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad json"})
		return
	}
	install, ok := cleanInstallID(in.InstallID)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	if in.Author == nil && in.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "empty"})
		return
	}
	c, status, reason := s.liveCommentForOwner(levelID, id, install)
	if status != 0 {
		if reason == "db error" {
			http.Error(w, "db error", status)
			return
		}
		writeJSON(w, status, map[string]any{"ok": false, "reason": reason})
		return
	}
	author := c.Author
	body := c.Body
	if in.Author != nil {
		author = cleanMRName(*in.Author, 16)
		if author == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad author"})
			return
		}
	}
	if in.Body != nil {
		body = fbClean(*in.Body, true)
		if body == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "empty"})
			return
		}
		if utf8.RuneCountInString(body) > commentMaxBodyRunes {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "too long"})
			return
		}
	}
	now := time.Now().UnixMilli()
	if _, err := s.store.db.Exec(
		`UPDATE manrocket_level_comments SET author=?, body=?, updated_at=? WHERE id=? AND level_id=? AND deleted_at IS NULL`,
		author, body, now, id, levelID,
	); err != nil {
		log.Printf("patch comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	c.Author = author
	c.Body = body
	c.UpdatedAt = now
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "comment": c.jsonMap(nil)})
}

func (s *server) mrDeleteComment(w http.ResponseWriter, r *http.Request, levelID string, id int64) {
	install, ok := commentActor(w, r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	if _, status, reason := s.liveCommentForOwner(levelID, id, install); status != 0 {
		if reason == "db error" {
			http.Error(w, "db error", status)
			return
		}
		writeJSON(w, status, map[string]any{"ok": false, "reason": reason})
		return
	}
	now := time.Now().UnixMilli()
	res, err := s.store.db.Exec(
		`UPDATE manrocket_level_comments SET deleted_at=?, updated_at=?
		 WHERE id=? AND level_id=? AND install_id=? AND deleted_at IS NULL`,
		now, now, id, levelID, install,
	)
	if err != nil {
		log.Printf("delete comment failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "reason": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true, "id": id})
}

// liveCommentForOwner loads a non-deleted comment on levelID. status 403 when
// install does not own it, 404 when it is missing or already soft-deleted.
func (s *server) liveCommentForOwner(levelID string, id int64, install string) (mrComment, int, string) {
	var c mrComment
	var deleted sql.NullInt64
	err := s.store.db.QueryRow(
		`SELECT id, level_id, install_id, author, body, reply_to, created_at, updated_at, deleted_at
		 FROM manrocket_level_comments WHERE id=? AND level_id=?`,
		id, levelID,
	).Scan(&c.ID, &c.LevelID, &c.InstallID, &c.Author, &c.Body, &c.ReplyTo, &c.CreatedAt, &c.UpdatedAt, &deleted)
	if err == sql.ErrNoRows || (err == nil && deleted.Valid) {
		return mrComment{}, http.StatusNotFound, "not found"
	}
	if err != nil {
		log.Printf("load comment failed: %v", err)
		return mrComment{}, http.StatusInternalServerError, "db error"
	}
	if c.InstallID != install {
		return mrComment{}, http.StatusForbidden, "forbidden"
	}
	return c, 0, ""
}

func (s *server) mrLevelExists(w http.ResponseWriter, levelID string) bool {
	var n int
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_levels WHERE id=?`, levelID).Scan(&n); err != nil {
		log.Printf("level lookup failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return false
	}
	if n == 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "reason": "not found"})
		return false
	}
	return true
}

func commentParentQuery(r *http.Request) (int64, bool, bool) {
	v := strings.TrimSpace(r.URL.Query().Get("reply_to"))
	if v == "" {
		v = strings.TrimSpace(r.URL.Query().Get("parent_id"))
	}
	if v == "" {
		return 0, false, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false, false
	}
	return n, true, true
}

// commentActor reads install_id from a JSON body, then from the query string.
// DELETE clients often cannot send a body; POST /delete can send either.
func commentActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, commentMaxBodyBytes)
		var in struct {
			InstallID string `json:"install_id"`
		}
		err := json.NewDecoder(r.Body).Decode(&in)
		if err == nil {
			if install, ok := cleanInstallID(in.InstallID); ok {
				return install, true
			}
			if strings.TrimSpace(in.InstallID) != "" {
				return "", false
			}
		}
	}
	return cleanInstallID(r.URL.Query().Get("install_id"))
}

type commentScanner interface {
	Scan(dest ...any) error
}

func scanComment(s commentScanner) (mrComment, error) {
	var c mrComment
	err := s.Scan(&c.ID, &c.LevelID, &c.InstallID, &c.Author, &c.Body, &c.ReplyTo, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func decodeJSONBody(r *http.Request, dest any) error {
	return json.NewDecoder(r.Body).Decode(dest)
}
