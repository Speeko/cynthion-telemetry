package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Public Spekks / side-game leaderboards. Submit is unauthenticated but
// allowlisted + rate-limited; reads are public for cynthiongame.com pages.

var allowedLeaderboardGames = map[string]bool{
	"manrocket":   true,
	"launchlaser": true,
}

var nameCleaner = regexp.MustCompile(`[^a-zA-Z0-9 _.\-]`)

func (s *server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		switch {
		case origin == "https://cynthiongame.com", origin == "https://www.cynthiongame.com":
			w.Header().Set("Access-Control-Allow-Origin", origin)
		case strings.HasPrefix(origin, "http://127.0.0.1:"), strings.HasPrefix(origin, "http://localhost:"):
			w.Header().Set("Access-Control-Allow-Origin", origin)
		case itchOrigin(origin):
			// the HTML5 build on itch.io runs from html-classic.itch.zone / *.itch.io
			w.Header().Set("Access-Control-Allow-Origin", origin)
		case origin == "":
			// non-browser clients (Godot HTTPRequest)
		default:
			// leave no ACAO for unknown origins on browser calls
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Admin-Key")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// itchOrigin: https origins on itch.io's game hosts (the web build's iframe).
func itchOrigin(origin string) bool {
	if !strings.HasPrefix(origin, "https://") {
		return false
	}
	host := strings.TrimPrefix(origin, "https://")
	if i := strings.IndexAny(host, ":/"); i >= 0 {
		host = host[:i]
	}
	return host == "itch.io" || host == "itch.zone" ||
		strings.HasSuffix(host, ".itch.io") || strings.HasSuffix(host, ".itch.zone")
}

type leaderboardSubmit struct {
	Game       string `json:"game"`
	Mode       string `json:"mode"` // full_run | level | course (ManRocket community course)
	Level      int    `json:"level"`
	Course     string `json:"course"` // mode=course: a manrocket_levels id
	TimeMS     int64  `json:"time_ms"`
	Name       string `json:"name"`
	InstallID  string `json:"install_id"`
	AppVersion string `json:"app_version"`
	Score      int64  `json:"score"`  // mode=score (Launch Laser 2)
	RunMS      int64  `json:"run_ms"` // mode=score: run length, for the plausibility check
	Kills      int64  `json:"kills"`  // mode=score
}

func (s *server) leaderboard(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.leaderboardGet(w, r)
	case http.MethodPost:
		s.leaderboardPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) leaderboardPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var in leaderboardSubmit
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	in.Game = strings.ToLower(strings.TrimSpace(in.Game))
	in.Mode = strings.ToLower(strings.TrimSpace(in.Mode))
	if !allowedLeaderboardGames[in.Game] {
		http.Error(w, "unknown game", http.StatusBadRequest)
		return
	}
	if !validLeaderboardMode(in.Game, in.Mode) {
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	if in.Mode == "score" {
		s.launchLaserPost(w, in)
		return
	}
	in.Course = strings.TrimSpace(in.Course)
	if in.Mode == "course" {
		in.Level = 0
		ok, err := s.store.manrocketCourseExists(in.Course)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "unknown course", http.StatusNotFound)
			return
		}
	} else {
		in.Course = ""
	}
	if in.Mode == "level" && (in.Level < 1 || in.Level > 99) {
		http.Error(w, "bad level", http.StatusBadRequest)
		return
	}
	if in.Mode == "full_run" {
		in.Level = 0
	}
	if in.TimeMS < 500 || in.TimeMS > 12*time.Hour.Milliseconds() {
		http.Error(w, "bad time", http.StatusBadRequest)
		return
	}
	name := cleanLeaderboardName(in.Name)
	install := strings.TrimSpace(in.InstallID)
	if install == "" || len(install) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	if in.Game == "manrocket" {
		owner, err := s.store.manrocketNameOwner(name)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if owner != "" && owner != install {
			http.Error(w, "name taken", http.StatusConflict)
			return
		}
	}

	res, err := s.store.db.Exec(
		`INSERT INTO leaderboard (received_at, game, mode, level, course, time_ms, name, install_id, app_version)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), in.Game, in.Mode, in.Level, in.Course, in.TimeMS, name, install, strings.TrimSpace(in.AppVersion),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

// cleanLeaderboardName: the one display-name cleaner (leaderboard rows + the ManRocket name registry).
func cleanLeaderboardName(raw string) string {
	name := strings.TrimSpace(raw)
	name = nameCleaner.ReplaceAllString(name, "")
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "anon"
	}
	if len(name) > 16 {
		name = strings.TrimSpace(name[:16])
	}
	// ensure printable
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, name)
}

func (s *server) leaderboardGet(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/leaderboard")
	path = strings.Trim(path, "/")
	game := path
	if game == "" {
		game = r.URL.Query().Get("game")
	}
	game = strings.ToLower(strings.TrimSpace(game))
	if !allowedLeaderboardGames[game] {
		http.Error(w, "unknown game", http.StatusBadRequest)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "" {
		mode = "full_run"
		if game == launchLaserGame {
			mode = "score"
		}
	}
	if !validLeaderboardMode(game, mode) {
		http.Error(w, "bad mode", http.StatusBadRequest)
		return
	}
	if mode == "score" {
		s.launchLaserGet(w, r)
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	level := 0
	if mode == "level" {
		n, err := strconv.Atoi(r.URL.Query().Get("level"))
		if err != nil || n < 1 {
			http.Error(w, "level required", http.StatusBadRequest)
			return
		}
		level = n
	}
	course := ""
	if mode == "course" {
		course = strings.TrimSpace(r.URL.Query().Get("course"))
		ok, err := s.store.manrocketCourseExists(course)
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "unknown course", http.StatusNotFound)
			return
		}
	}

	rows, err := s.store.queryLeaderboard(game, mode, level, course, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"game":    game,
		"mode":    mode,
		"level":   level,
		"course":  course,
		"entries": rows,
	})
}

// validLeaderboardMode: ManRocket has full_run / level / course (community courses); Launch Laser 2
// has only score (highest wins).
func validLeaderboardMode(game, mode string) bool {
	if game == launchLaserGame {
		return mode == "score"
	}
	switch mode {
	case "full_run", "level":
		return true
	case "course":
		return game == "manrocket"
	}
	return false
}

// manrocketCourseExists: a course board only exists for a course on the community server.
func (st *Store) manrocketCourseExists(id string) (bool, error) {
	if id == "" || len(id) > 80 {
		return false, nil
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(1) FROM manrocket_levels WHERE id=?`, id).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ensureLeaderboardCourseColumn: the live leaderboard table predates community-course boards, and
// CREATE TABLE IF NOT EXISTS never adds columns — add `course` once ("duplicate column" = done).
func (st *Store) ensureLeaderboardCourseColumn() error {
	if _, err := st.db.Exec(`ALTER TABLE leaderboard ADD COLUMN course TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	_, err := st.db.Exec(`CREATE INDEX IF NOT EXISTS idx_lb_game_mode_course_time ON leaderboard(game, mode, course, time_ms)`)
	return err
}

func (st *Store) queryLeaderboard(game, mode string, level int, course string, limit int) ([]map[string]any, error) {
	q := `SELECT name, time_ms, level, received_at, app_version, COALESCE(install_id, '')
	      FROM leaderboard WHERE game=? AND mode=?`
	args := []any{game, mode}
	switch mode {
	case "level":
		q += ` AND level=?`
		args = append(args, level)
	case "course":
		// one row per install (its best): a community board is small, one grinder shouldn't fill it.
		// SQLite bare columns next to a lone MIN() come from the row that holds the minimum.
		q = `SELECT name, MIN(time_ms) AS t, level, received_at, app_version, install_id
		     FROM leaderboard WHERE game=? AND mode=? AND course=? GROUP BY install_id`
		args = append(args, course)
	}
	if mode == "course" {
		q += ` ORDER BY t ASC, received_at ASC LIMIT ?`
	} else {
		q += ` ORDER BY time_ms ASC LIMIT ?`
	}
	args = append(args, limit)
	rs, err := st.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []map[string]any{}
	rank := 1
	for rs.Next() {
		var name, ver, install string
		var timeMS, lvl, received int64
		if err := rs.Scan(&name, &timeMS, &lvl, &received, &ver, &install); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"rank":        rank,
			"name":        name,
			"time_ms":     timeMS,
			"level":       lvl,
			"received_at": received,
			"app_version": ver,
			"tag":         installTag(install),
		})
		rank++
	}
	return out, rs.Err()
}

// installTag: a short stable tag per install so same-named pilots can be told apart (never the id).
func installTag(install string) string {
	if install == "" {
		return ""
	}
	return sha1Hex4("manrocket-tag:" + install)
}

func sha1Hex4(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])[:4]
}
