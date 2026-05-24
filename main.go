// Cynthion telemetry ingest endpoint.
//
// Two write endpoints (POST /v1/events, POST /v1/crash) + a /health probe.
// SQLite storage (modernc.org/sqlite — pure Go, no CGO).
// Auth: X-API-Key header against INGEST_API_KEY env var.
// Rate limit: per-IP token bucket via golang.org/x/time/rate.
//
// Designed for the cynthion-au droplet:
//   - Listens on :8090 inside Docker, port mapped to 127.0.0.1:8090 on host
//   - Caddy (in /srv/asteroids-mmo/) reverse-proxies api.cynthiongame.com -> host.docker.internal:8090
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
	"golang.org/x/time/rate"
)

const (
	maxBodyBytes      = 4 << 20  // 4 MB — boot logs run a few KB but allow headroom
	maxEventsPerBatch = 200
)

type Config struct {
	Listen string
	APIKey string
	DBPath string
}

func loadConfig() Config {
	c := Config{
		Listen: envOr("LISTEN_ADDR", ":8090"),
		APIKey: os.Getenv("INGEST_API_KEY"),
		DBPath: envOr("DB_PATH", "/data/events.db"),
	}
	if c.APIKey == "" {
		log.Fatal("INGEST_API_KEY env var is required")
	}
	if len(c.APIKey) < 24 {
		log.Fatal("INGEST_API_KEY must be at least 24 characters")
	}
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ------------- store -------------

const schema = `
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    client_ts INTEGER,
    install_id TEXT NOT NULL,
    session_id TEXT,
    app_version TEXT,
    os TEXT,
    gpu TEXT,
    event_type TEXT NOT NULL,
    payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_install ON events(install_id);
CREATE INDEX IF NOT EXISTS idx_events_received ON events(received_at);
CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);

CREATE TABLE IF NOT EXISTS crashes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    received_at INTEGER NOT NULL,
    install_id TEXT NOT NULL,
    session_id TEXT,
    app_version TEXT,
    os TEXT,
    gpu TEXT,
    error_summary TEXT,
    boot_log TEXT,
    player_log TEXT,
    payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_crashes_install ON crashes(install_id);
CREATE INDEX IF NOT EXISTS idx_crashes_received ON crashes(received_at);
`

type Store struct{ db *sql.DB }

func openStore(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Event struct {
	ClientTS  int64           `json:"client_ts"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type EventsBatch struct {
	InstallID  string  `json:"install_id"`
	SessionID  string  `json:"session_id"`
	AppVersion string  `json:"app_version"`
	OS         string  `json:"os"`
	GPU        string  `json:"gpu"`
	Events     []Event `json:"events"`
}

type Crash struct {
	InstallID    string          `json:"install_id"`
	SessionID    string          `json:"session_id"`
	AppVersion   string          `json:"app_version"`
	OS           string          `json:"os"`
	GPU          string          `json:"gpu"`
	ErrorSummary string          `json:"error_summary"`
	BootLog      string          `json:"boot_log,omitempty"`
	PlayerLog    string          `json:"player_log,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

func (s *Store) insertEvents(ctx context.Context, b EventsBatch, receivedAt int64) (int, error) {
	if len(b.Events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO events
        (received_at, client_ts, install_id, session_id, app_version, os, gpu, event_type, payload)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, e := range b.Events {
		payload := string(e.Payload)
		if payload == "" {
			payload = "{}"
		}
		if _, err := stmt.ExecContext(ctx,
			receivedAt, e.ClientTS, b.InstallID, b.SessionID, b.AppVersion, b.OS, b.GPU,
			e.EventType, payload); err != nil {
			return n, err
		}
		n++
	}
	return n, tx.Commit()
}

func (s *Store) insertCrash(ctx context.Context, c Crash, receivedAt int64) (int64, error) {
	payload := string(c.Payload)
	if payload == "" {
		payload = "{}"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO crashes
        (received_at, install_id, session_id, app_version, os, gpu, error_summary, boot_log, player_log, payload)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receivedAt, c.InstallID, c.SessionID, c.AppVersion, c.OS, c.GPU,
		c.ErrorSummary, c.BootLog, c.PlayerLog, payload)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ------------- middleware -------------

type ipLimiter struct {
	mu      sync.Mutex
	clients map[string]*rate.Limiter
	r       rate.Limit
	burst   int
}

func newIPLimiter(rps float64, burst int) *ipLimiter {
	return &ipLimiter{
		clients: make(map[string]*rate.Limiter),
		r:       rate.Limit(rps),
		burst:   burst,
	}
}

func (il *ipLimiter) get(ip string) *rate.Limiter {
	il.mu.Lock()
	defer il.mu.Unlock()
	lim, ok := il.clients[ip]
	if !ok {
		lim = rate.NewLimiter(il.r, il.burst)
		il.clients[ip] = lim
	}
	return lim
}

func clientIP(r *http.Request) string {
	// Caddy sets X-Forwarded-For; trust it because we're behind our own reverse proxy.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return xff[:i]
			}
		}
		return xff
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

func (s *server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != s.cfg.APIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) withRateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.get(clientIP(r)).Allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func (s *server) withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(ww, r)
		log.Printf("%s %s %d %s %dms ua=%q",
			clientIP(r), r.Method+" "+r.URL.Path, ww.status,
			r.UserAgent(), time.Since(start).Milliseconds(), r.UserAgent())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

// ------------- handlers -------------

type server struct {
	cfg     Config
	store   *Store
	limiter *ipLimiter
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true,"service":"cynthion-telemetry"}`))
}

func (s *server) ingestEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var b EventsBatch
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.InstallID == "" {
		http.Error(w, "install_id required", http.StatusBadRequest)
		return
	}
	if len(b.Events) > maxEventsPerBatch {
		http.Error(w, "too many events in batch", http.StatusBadRequest)
		return
	}
	n, err := s.store.insertEvents(r.Context(), b, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert events failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "received": n})
}

func (s *server) ingestCrash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var c Crash
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if c.InstallID == "" {
		http.Error(w, "install_id required", http.StatusBadRequest)
		return
	}
	id, err := s.store.insertCrash(r.Context(), c, time.Now().UnixMilli())
	if err != nil {
		log.Printf("insert crash failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// ------------- main -------------

func main() {
	cfg := loadConfig()
	store, err := openStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	defer store.Close()

	s := &server{
		cfg:     cfg,
		store:   store,
		limiter: newIPLimiter(2, 20), // 2 req/sec/IP sustained, burst 20
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/events", s.withRateLimit(s.withAuth(s.ingestEvents)))
	mux.HandleFunc("/v1/crash", s.withRateLimit(s.withAuth(s.ingestCrash)))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.withLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		log.Printf("cynthion-telemetry listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
