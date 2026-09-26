package main

// ManRocket pilot-name registry: one install per name (case-insensitive, leaderboard cleaning).
//
//   GET  /v1/manrocket/names/check?name=..&install_id=.. -> {ok, available, reason, name}
//   POST /v1/manrocket/names/claim {name, install_id}    -> {ok, name} | 409 {ok:false, reason:"taken"}
//
// Owner of a name = the install that FIRST posted it to the manrocket leaderboard (so every name
// already on the board stays with whoever set those times, renames or not); failing that, the
// install holding a claim in manrocket_names. "anon" is the shared default and never owned.
// A claim releases the install's previous claim (rename). The leaderboard POST 409s a taken name.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const namesSchema = `
CREATE TABLE IF NOT EXISTS manrocket_names (
	norm TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	install_id TEXT NOT NULL,
	claimed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mr_names_install ON manrocket_names(install_id);
CREATE INDEX IF NOT EXISTS idx_lb_game_lname ON leaderboard(game, lower(name), received_at);
`

func (s *Store) ensureNamesSchema() error {
	_, err := s.db.Exec(namesSchema)
	return err
}

func normName(clean string) string { return strings.ToLower(clean) }

// manrocketNameOwner returns the install that owns a (cleaned) name, or "" when it's free / "anon".
func (s *Store) manrocketNameOwner(clean string) (string, error) {
	norm := normName(clean)
	if norm == "anon" {
		return "", nil
	}
	var owner string
	err := s.db.QueryRow(
		`SELECT install_id FROM leaderboard WHERE game='manrocket' AND lower(name)=? ORDER BY received_at ASC LIMIT 1`,
		norm).Scan(&owner)
	if err == nil {
		return owner, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	err = s.db.QueryRow(`SELECT install_id FROM manrocket_names WHERE norm=?`, norm).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return owner, err
}

func (s *server) manrocketNames(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/manrocket/names"), "/")
	switch {
	case path == "check" && r.Method == http.MethodGet:
		s.namesCheck(w, r, r.URL.Query().Get("name"), r.URL.Query().Get("install_id"), false)
	case path == "claim" && r.Method == http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
		var in struct {
			Name      string `json:"name"`
			InstallID string `json:"install_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		s.namesCheck(w, r, in.Name, in.InstallID, true)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *server) namesCheck(w http.ResponseWriter, r *http.Request, rawName, rawInstall string, claim bool) {
	install := strings.TrimSpace(rawInstall)
	if install == "" || len(install) > 64 {
		http.Error(w, "bad install_id", http.StatusBadRequest)
		return
	}
	name := cleanLeaderboardName(rawName)
	owner, err := s.store.manrocketNameOwner(name)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if owner != "" && owner != install {
		status := http.StatusOK
		if claim {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"ok": !claim, "available": false, "reason": "taken", "name": name})
		return
	}
	if claim && normName(name) != "anon" {
		tx, err := s.store.db.Begin()
		if err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`DELETE FROM manrocket_names WHERE install_id=? AND norm<>?`, install, normName(name)); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if _, err := tx.Exec(
			`INSERT INTO manrocket_names (norm, name, install_id, claimed_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT(norm) DO UPDATE SET name=excluded.name WHERE manrocket_names.install_id=excluded.install_id`,
			normName(name), name, install, time.Now().UnixMilli()); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		// lost a race to another install's claim
		if o, err := s.store.manrocketNameOwner(name); err == nil && o != install {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "available": false, "reason": "taken", "name": name})
			return
		}
	} else if claim {
		// renaming back to "anon" releases any claim
		if _, err := s.store.db.Exec(`DELETE FROM manrocket_names WHERE install_id=?`, install); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "available": true, "reason": "ok", "name": name})
}
