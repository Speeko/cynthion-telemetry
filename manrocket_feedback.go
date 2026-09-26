package main

// ManRocket feedback: the in-game "New Message" window (a fake Outlook Express 98 compose box).
// Nothing is emailed — rows land in SQLite and the Director reads them via the admin GET.
//
//   POST /v1/manrocket/feedback  {from, subject, body, cc[], pilot, version, platform, install_id} -> {ok, id}
//   GET  /v1/manrocket/feedback?limit=N  X-Admin-Key -> {ok, feedback:[...]} newest first (N<=200, default 50)

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	fbMaxSubject   = 120  // runes
	fbMaxBody      = 4000 // runes
	fbMaxFrom      = 64
	fbMaxCC        = 20
	fbMaxCCEntry   = 64
	fbMaxBodyBytes = 64 << 10
	fbDailyInstall = 10
	fbDailyIP      = 30
	fbListMaxLimit = 200
	fbListDefLimit = 50
)

const fbSchema = `
CREATE TABLE IF NOT EXISTS manrocket_feedback (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	received_at INTEGER NOT NULL,
	from_str TEXT NOT NULL,
	subject TEXT NOT NULL,
	body TEXT NOT NULL,
	cc TEXT NOT NULL,
	pilot TEXT NOT NULL,
	version TEXT NOT NULL,
	platform TEXT NOT NULL,
	install_id TEXT NOT NULL,
	ip TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mr_fb_install ON manrocket_feedback(install_id, received_at);
CREATE INDEX IF NOT EXISTS idx_mr_fb_ip ON manrocket_feedback(ip, received_at);
`

func (s *Store) ensureFeedbackSchema() error {
	_, err := s.db.Exec(fbSchema)
	return err
}

func (s *server) manrocketFeedback(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/manrocket/feedback"), "/")
	switch {
	case path == "" && r.Method == http.MethodPost:
		s.fbSubmit(w, r)
	case path == "" && r.Method == http.MethodGet:
		s.fbList(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// fbClean strips control chars (keeping newlines/tabs when multiline) and trims.
func fbClean(s string, multiline bool) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r':
			return -1
		case multiline && (r == '\n' || r == '\t'):
			return r
		case unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// fbCap truncates to max runes (for fields where overflow is harmless: from, pilot, version...).
func fbCap(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

func (s *server) fbSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, fbMaxBodyBytes)
	var in struct {
		From      string   `json:"from"`
		Subject   string   `json:"subject"`
		Body      string   `json:"body"`
		CC        []string `json:"cc"`
		Pilot     string   `json:"pilot"`
		Version   string   `json:"version"`
		Platform  string   `json:"platform"`
		InstallID string   `json:"install_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad json"})
		return
	}
	install := strings.TrimSpace(in.InstallID)
	if install == "" || len(install) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "bad install_id"})
		return
	}
	subject := fbClean(in.Subject, false)
	body := fbClean(in.Body, true)
	if subject == "" && body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "empty"})
		return
	}
	if utf8.RuneCountInString(subject) > fbMaxSubject || utf8.RuneCountInString(body) > fbMaxBody {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": "too long"})
		return
	}
	cc := []string{}
	for _, c := range in.CC {
		if len(cc) >= fbMaxCC {
			break
		}
		if c = fbCap(fbClean(c, false), fbMaxCCEntry); c != "" {
			cc = append(cc, c)
		}
	}
	ccJSON, _ := json.Marshal(cc)

	ip := ipTag(r)
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var byInstall, byIP int
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_feedback WHERE install_id=? AND received_at>?`, install, since).Scan(&byInstall); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_feedback WHERE ip=? AND received_at>?`, ip, since).Scan(&byIP); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if byInstall >= fbDailyInstall || byIP >= fbDailyIP {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "reason": "daily feedback limit reached"})
		return
	}
	res, err := s.store.db.Exec(
		`INSERT INTO manrocket_feedback (received_at, from_str, subject, body, cc, pilot, version, platform, install_id, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(),
		fbCap(fbClean(in.From, false), fbMaxFrom), subject, body, string(ccJSON),
		cleanMRName(in.Pilot, 16), fbCap(fbClean(in.Version, false), 32), fbCap(fbClean(in.Platform, false), 32),
		install, ip,
	)
	if err != nil {
		log.Printf("insert feedback failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// fbList is the Director's inbox. Needs MANROCKET_ADMIN_KEY in X-Admin-Key.
func (s *server) fbList(w http.ResponseWriter, r *http.Request) {
	key := os.Getenv("MANROCKET_ADMIN_KEY")
	got := r.Header.Get("X-Admin-Key")
	if len(key) < 24 || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	limit := fbListDefLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, fbListMaxLimit)
		}
	}
	rows, err := s.store.db.Query(
		`SELECT id, received_at, from_str, subject, body, cc, pilot, version, platform, install_id
		 FROM manrocket_feedback ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, at int64
		var from, subject, body, ccRaw, pilot, version, platform, install string
		if err := rows.Scan(&id, &at, &from, &subject, &body, &ccRaw, &pilot, &version, &platform, &install); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		var cc []string
		_ = json.Unmarshal([]byte(ccRaw), &cc)
		out = append(out, map[string]any{
			"id": id, "received_at": at, "from": from, "subject": subject, "body": body, "cc": cc,
			"pilot": pilot, "version": version, "platform": platform, "install_id": install,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "feedback": out})
}
