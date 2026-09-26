package main

// ManRocket meeting-room phrases: a player who uploaded a level may add ONE short line that the
// coworkers say in the cold-open chatter. The server renders it as Microsoft Sam (SAPI4, tetyys.com)
// at the four seat pitches the game uses and stores the wavs.
//
//   POST /v1/manrocket/phrases {text, name, install_id} -> {ok, id, text, wavs:{"70":url,..}}
//        one phrase per install (resubmit replaces it; new id). 400 bad/blocked text, 429 caps.
//   GET  /v1/manrocket/phrases/random?limit=N (N<=30, default 20) -> {ok, phrases:[{id,text,name,wavs}]}
//   GET  /v1/manrocket/phrases/<id>/<pitch>.wav -> audio/wav (immutable: a new submit = new id)

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var phrasePitches = []int{70, 100, 130, 165}

const (
	phraseMaxLen       = 40
	phraseMaxWavBytes  = 400 << 10
	phraseDailyInstall = 6  // submits (renders) per install per 24 h
	phraseDailyIP      = 15 // per IP-hash per 24 h
	phraseTTSURL       = "https://www.tetyys.com/SAPI4/SAPI4"
	phraseTTSSpeed     = 165
)

var phraseAllowed = regexp.MustCompile(`^[A-Za-z0-9 .,!?'\-]+$`)

// phraseBlocklist: normalised (lowercase, leet folded, letters only). Short/common words match whole
// words only (so "assess", "scunthorpe" pass); long, unambiguous ones match anywhere.
var phraseBlockWords = []string{
	"fuck", "fucker", "fucking", "fuckin", "shit", "cunt", "cunts", "cock", "dick", "dicks", "pussy",
	"twat", "wank", "wanker", "bitch", "bitches", "whore", "slut", "rape", "raping", "rapist",
	"fag", "fags", "tranny", "spic", "spics", "chink", "chinks", "gook", "kike", "kikes", "coon",
	"coons", "wog", "wogs", "paki", "pakis", "dyke", "retard", "retards", "abo", "abos", "nazi",
	"hitler", "cum", "porn", "anal",
}
var phraseBlockSubstr = []string{
	"nigger", "nigga", "niggah", "faggot", "retarded", "motherfuck", "fuckyou", "kys", "killyourself",
	"heilhitler", "siegheil", "whitepower", "gasthe", "cocksuck", "blowjob", "wetback", "raghead",
	"sandnigger", "beaner", "towelhead", "shemale",
}

var leetFold = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s", "!", "i")

// cleanPhrase normalises spacing and validates length/charset/blocklist; returns (text, reason).
func cleanPhrase(raw string) (string, string) {
	t := strings.Join(strings.Fields(raw), " ")
	if t == "" {
		return "", "empty"
	}
	if len(t) > phraseMaxLen {
		return "", "too long"
	}
	if !phraseAllowed.MatchString(t) {
		return "", "letters, numbers and . , ! ? ' - only"
	}
	if phraseBlocked(t) {
		return "", "blocked"
	}
	return t, ""
}

func phraseBlocked(t string) bool {
	low := leetFold.Replace(strings.ToLower(t))
	words := strings.FieldsFunc(low, func(r rune) bool { return r < 'a' || r > 'z' })
	joined := strings.Join(words, "")
	for _, w := range words {
		for _, b := range phraseBlockWords {
			if w == b {
				return true
			}
		}
	}
	for _, b := range phraseBlockSubstr {
		if strings.Contains(joined, b) {
			return true
		}
	}
	return false
}

const phrasesSchema = `
CREATE TABLE IF NOT EXISTS manrocket_phrases (
	install_id TEXT PRIMARY KEY,
	id TEXT NOT NULL UNIQUE,
	text TEXT NOT NULL,
	name TEXT NOT NULL,
	ip TEXT NOT NULL,
	updated_at INTEGER NOT NULL,
	hidden INTEGER NOT NULL DEFAULT 0,
	wav70 BLOB NOT NULL,
	wav100 BLOB NOT NULL,
	wav130 BLOB NOT NULL,
	wav165 BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS manrocket_phrase_submits (
	install_id TEXT NOT NULL,
	ip TEXT NOT NULL,
	at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mr_phrase_submits ON manrocket_phrase_submits(at);
`

func (s *Store) ensurePhrasesSchema() error {
	_, err := s.db.Exec(phrasesSchema)
	return err
}

var ttsClient = &http.Client{Timeout: 40 * time.Second}

func renderSam(ctx context.Context, text string, pitch int) ([]byte, error) {
	q := url.Values{}
	q.Set("text", text)
	q.Set("voice", "Sam")
	q.Set("pitch", strconv.Itoa(pitch))
	q.Set("speed", strconv.Itoa(phraseTTSSpeed))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, phraseTTSURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := ttsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tts status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, phraseMaxWavBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > phraseMaxWavBytes {
		return nil, fmt.Errorf("tts wav too large")
	}
	if len(body) < 44 || string(body[0:4]) != "RIFF" || string(body[8:12]) != "WAVE" {
		return nil, fmt.Errorf("tts returned non-wav")
	}
	return body, nil
}

// Wav fetches get their own, looser per-IP bucket: a boot cache refresh pulls up to ~4 wavs per phrase.
var phraseWavLimiter = newIPLimiter(20, 160)

func (s *server) manrocketPhrases(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/manrocket/phrases"), "/")
	lim := s.limiter
	if strings.HasSuffix(path, ".wav") && r.Method == http.MethodGet {
		lim = phraseWavLimiter
	}
	if !lim.get(clientIP(r)).Allow() {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	switch {
	case path == "" && r.Method == http.MethodPost:
		s.phraseSubmit(w, r)
	case path == "random" && r.Method == http.MethodGet:
		s.phraseRandom(w, r)
	case strings.HasSuffix(path, ".wav") && r.Method == http.MethodGet:
		s.phraseWav(w, r, path)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func phraseWavURLs(id string) map[string]string {
	out := map[string]string{}
	for _, p := range phrasePitches {
		out[strconv.Itoa(p)] = fmt.Sprintf("/v1/manrocket/phrases/%s/%d.wav", id, p)
	}
	return out
}

func (s *server) phraseSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<12)
	var in struct {
		Text      string `json:"text"`
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
	text, reason := cleanPhrase(in.Text)
	if reason != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "reason": reason})
		return
	}
	name := cleanLeaderboardName(in.Name)
	ip := ipTag(r)
	since := time.Now().Add(-24 * time.Hour).UnixMilli()
	var byInstall, byIP int
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_phrase_submits WHERE install_id=? AND at>?`, install, since).Scan(&byInstall); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if err := s.store.db.QueryRow(`SELECT COUNT(1) FROM manrocket_phrase_submits WHERE ip=? AND at>?`, ip, since).Scan(&byIP); err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if byInstall >= phraseDailyInstall || byIP >= phraseDailyIP {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "reason": "daily limit reached"})
		return
	}
	now := time.Now().UnixMilli()
	_, _ = s.store.db.Exec(`INSERT INTO manrocket_phrase_submits (install_id, ip, at) VALUES (?, ?, ?)`, install, ip, now)

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	// Sequential: tetyys.com 503s concurrent requests from one client. Retry a 503 a couple of times.
	wavs := make([][]byte, len(phrasePitches))
	for i, p := range phrasePitches {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if wavs[i], err = renderSam(ctx, text, p); err == nil {
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(1500*(attempt+1)) * time.Millisecond):
			}
		}
		if err != nil {
			log.Printf("phrase tts failed (pitch %d): %v", p, err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "reason": "voice render failed, try again"})
			return
		}
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	id := hex.EncodeToString(idb)
	if _, err := s.store.db.Exec(
		`INSERT INTO manrocket_phrases (install_id, id, text, name, ip, updated_at, hidden, wav70, wav100, wav130, wav165)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)
		 ON CONFLICT(install_id) DO UPDATE SET id=excluded.id, text=excluded.text, name=excluded.name, ip=excluded.ip,
		   updated_at=excluded.updated_at, wav70=excluded.wav70, wav100=excluded.wav100, wav130=excluded.wav130, wav165=excluded.wav165`,
		install, id, text, name, ip, now, wavs[0], wavs[1], wavs[2], wavs[3]); err != nil {
		log.Printf("insert phrase failed: %v", err)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "text": text, "wavs": phraseWavURLs(id)})
}

func (s *server) phraseRandom(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 30)
		}
	}
	rows, err := s.store.db.Query(`SELECT id, text, name FROM manrocket_phrases WHERE hidden=0 ORDER BY RANDOM() LIMIT ?`, limit)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, text, name string
		if err := rows.Scan(&id, &text, &name); err != nil {
			http.Error(w, "db error", http.StatusInternalServerError)
			return
		}
		out = append(out, map[string]any{"id": id, "text": text, "name": name, "wavs": phraseWavURLs(id)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "phrases": out})
}

// path: "<id>/<pitch>.wav"
func (s *server) phraseWav(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.TrimSuffix(path, ".wav"), "/")
	if len(parts) != 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	col := ""
	for _, p := range phrasePitches {
		if parts[1] == strconv.Itoa(p) {
			col = "wav" + parts[1]
		}
	}
	if col == "" || len(parts[0]) != 16 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var wav []byte
	if err := s.store.db.QueryRow(`SELECT `+col+` FROM manrocket_phrases WHERE id=? AND hidden=0`, parts[0]).Scan(&wav); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(wav)
}
