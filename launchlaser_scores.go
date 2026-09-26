package main

// Launch Laser 2 high scores: game "launchlaser", mode "score". Highest wins, one row per install
// (its best) per board, boards for all time / the last 7 days / today (UTC).
//
//   POST /v1/leaderboard {game:"launchlaser", mode:"score", score, run_ms, kills, name, install_id, app_version}
//   GET  /v1/leaderboard/launchlaser?mode=score&period=all|week|day[&limit=N][&install_id=ID]
//        -> {ok, game, mode, period, entries:[{rank, name, tag, score, received_at, app_version}], you:{rank, score}|null}
//
// Rows share the leaderboard table: the score lives in the `score` column (added once by
// ensureLeaderboardScoreColumn), time_ms holds the run length.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	launchLaserMaxScore = 1_000_000
	launchLaserGame     = "launchlaser"
)

// ensureLeaderboardScoreColumn: like the course column — CREATE TABLE IF NOT EXISTS never adds columns.
func (st *Store) ensureLeaderboardScoreColumn() error {
	if _, err := st.db.Exec(`ALTER TABLE leaderboard ADD COLUMN score INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	_, err := st.db.Exec(`CREATE INDEX IF NOT EXISTS idx_lb_game_mode_score ON leaderboard(game, mode, score)`)
	return err
}

// plausibleScore rejects obviously forged runs without punishing real ones: at most ~8 points a
// second (a Mothership is worth 15-30 at once, hence the +60 slack), and never more than 40 per kill.
func plausibleScore(score, runMS, kills int64) bool {
	if score < 1 || score > launchLaserMaxScore {
		return false
	}
	if runMS > 0 && score > runMS/1000*8+60 {
		return false
	}
	if kills > 0 && score > kills*40 {
		return false
	}
	return true
}

func (s *server) launchLaserPost(w http.ResponseWriter, in leaderboardSubmit) {
	if !plausibleScore(in.Score, in.RunMS, in.Kills) {
		http.Error(w, "bad score", http.StatusBadRequest)
		return
	}
	name := cleanLeaderboardName(in.Name)
	install := strings.TrimSpace(in.InstallID)
	if install == "" || len(install) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	runMS := in.RunMS
	if runMS < 0 || runMS > 12*time.Hour.Milliseconds() {
		runMS = 0
	}
	res, err := s.store.db.Exec(
		`INSERT INTO leaderboard (received_at, game, mode, level, course, time_ms, score, name, install_id, app_version)
		 VALUES (?, ?, 'score', 0, '', ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), launchLaserGame, runMS, in.Score, name, install, strings.TrimSpace(in.AppVersion),
	)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	id, _ := res.LastInsertId()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id})
}

// periodStart: the earliest received_at (ms) a board counts. "day" = since 00:00 UTC today.
func periodStart(period string, now time.Time) int64 {
	switch period {
	case "day":
		y, m, d := now.UTC().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).UnixMilli()
	case "week":
		return now.Add(-7 * 24 * time.Hour).UnixMilli()
	}
	return 0
}

func (s *server) launchLaserGet(w http.ResponseWriter, r *http.Request) {
	period := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("period")))
	if period != "day" && period != "week" {
		period = "all"
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	since := periodStart(period, time.Now())
	rows, err := s.store.queryScoreBoard(launchLaserGame, since, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var you any
	if install := strings.TrimSpace(r.URL.Query().Get("install_id")); install != "" && len(install) <= 64 {
		if rank, best, ok, err := s.store.scoreRank(launchLaserGame, since, install); err == nil && ok {
			you = map[string]any{"rank": rank, "score": best}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"game":    launchLaserGame,
		"mode":    "score",
		"period":  period,
		"entries": rows,
		"you":     you,
	})
}

func (st *Store) queryScoreBoard(game string, since int64, limit int) ([]map[string]any, error) {
	// One row per install: SQLite bare columns next to a lone MAX() come from the row holding it.
	rs, err := st.db.Query(
		`SELECT name, MAX(score) AS s, received_at, app_version, install_id
		 FROM leaderboard WHERE game=? AND mode='score' AND received_at>=?
		 GROUP BY install_id ORDER BY s DESC, received_at ASC LIMIT ?`, game, since, limit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []map[string]any{}
	rank := 1
	for rs.Next() {
		var name, ver, install string
		var score, received int64
		if err := rs.Scan(&name, &score, &received, &ver, &install); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"rank":        rank,
			"name":        name,
			"score":       score,
			"received_at": received,
			"app_version": ver,
			"tag":         gameTag(game, install),
		})
		rank++
	}
	return out, rs.Err()
}

// scoreRank: this install's best on the board and its rank (1 + installs with a strictly higher best).
func (st *Store) scoreRank(game string, since int64, install string) (rank int, best int64, ok bool, err error) {
	var b *int64
	if err = st.db.QueryRow(
		`SELECT MAX(score) FROM leaderboard WHERE game=? AND mode='score' AND received_at>=? AND install_id=?`,
		game, since, install).Scan(&b); err != nil || b == nil {
		return 0, 0, false, err
	}
	var higher int
	if err = st.db.QueryRow(
		`SELECT COUNT(1) FROM (SELECT MAX(score) AS s FROM leaderboard WHERE game=? AND mode='score' AND received_at>=?
		 GROUP BY install_id) WHERE s > ?`, game, since, *b).Scan(&higher); err != nil {
		return 0, 0, false, err
	}
	return higher + 1, *b, true, nil
}

// gameTag: the per-install tag, salted per game (ManRocket keeps its original salt so tags don't change).
func gameTag(game, install string) string {
	if game == "manrocket" {
		return installTag(install)
	}
	if install == "" {
		return ""
	}
	return sha1Hex4(game + "-tag:" + install)
}
