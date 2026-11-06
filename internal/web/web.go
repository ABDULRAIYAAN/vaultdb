// Package web serves the VaultDB playground: a website where anyone can try
// VaultDB from their browser.
//
// Every visitor gets a private, temporary, in-memory database (a sandbox),
// so nobody can see or break anyone else's data, and nothing touches disk.
// Sandboxes are deleted after a period of inactivity.
//
// Endpoints (all JSON):
//
//	POST /api/exec    {"command": "SET a 1"}  -> {"reply": "OK", "micros": 12, "keys": 1, "bytes": 30}
//	POST /api/reset   wipe your sandbox
//	GET  /api/stats   live numbers about the server itself
//	POST /api/ask     {"question": "..."}     -> answer + sources from VaultDB's own docs
//	GET  /            the site itself (static files)
package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ABDULRAIYAAN/vaultdb/internal/engine"
	"github.com/ABDULRAIYAAN/vaultdb/internal/rag"
	"github.com/ABDULRAIYAAN/vaultdb/internal/store"
)

// Options configures the site.
type Options struct {
	Static        fs.FS         // the built front end
	RAG           *rag.RAG      // nil disables /api/ask
	MaxSessions   int           // sandboxes kept at once
	SessionTTL    time.Duration // idle time before a sandbox is deleted
	MaxDataBytes  int64         // data allowed per sandbox
	TrustProxy    bool          // take the visitor IP from X-Forwarded-For (only behind your own proxy)
	AskPerDay     int           // total /api/ask calls allowed per day (protects a paid API key)
	SecureCookies bool          // set when served over HTTPS
}

// Server is the playground HTTP handler.
type Server struct {
	opts Options
	mux  *http.ServeMux

	mu       sync.Mutex
	sessions map[string]*sandbox

	execLimit *limiter
	askLimit  *limiter

	started  time.Time
	commands atomic.Int64
	askDay   string
	askCount int
}

type sandbox struct {
	mu       sync.Mutex // one command at a time per visitor
	st       *store.Store
	eng      *engine.Engine
	lastUsed time.Time
}

// New builds the server. Call Close when done to stop the cleanup goroutine.
func New(opts Options) *Server {
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = 2000
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 30 * time.Minute
	}
	if opts.MaxDataBytes <= 0 {
		opts.MaxDataBytes = 1 << 20
	}
	if opts.AskPerDay <= 0 {
		opts.AskPerDay = 500
	}
	s := &Server{
		opts:      opts,
		mux:       http.NewServeMux(),
		sessions:  map[string]*sandbox{},
		execLimit: newLimiter(10, 30), // 10 commands per second, bursts of 30
		askLimit:  newLimiter(0.1, 5), // one question every 10 seconds, bursts of 5
		started:   time.Now(),
	}
	s.mux.HandleFunc("POST /api/exec", s.handleExec)
	s.mux.HandleFunc("POST /api/reset", s.handleReset)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)
	s.mux.HandleFunc("POST /api/ask", s.handleAsk)
	if opts.Static != nil {
		s.mux.Handle("GET /", staticHandler(opts.Static))
	}
	return s
}

// ServeHTTP adds security headers and a body size limit to every request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	if s.opts.SecureCookies {
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	s.mux.ServeHTTP(w, r)
}

// Run serves on addr until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second, // answers from an AI model can take a while
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go s.janitor(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// janitor deletes idle sandboxes and stale rate-limit entries every minute.
func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(time.Now())
		}
	}
}

func (s *Server) sweep(now time.Time) {
	s.mu.Lock()
	for id, sb := range s.sessions {
		sb.mu.Lock()
		idle := now.Sub(sb.lastUsed) > s.opts.SessionTTL
		sb.mu.Unlock()
		if idle {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	s.execLimit.sweep(now)
	s.askLimit.sweep(now)
}

const cookieName = "vaultdb_sandbox"

// sandboxFor returns the visitor's sandbox, creating one (and setting the
// cookie) if they don't have one yet.
func (s *Server) sandboxFor(w http.ResponseWriter, r *http.Request) (*sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, err := r.Cookie(cookieName); err == nil {
		if sb, ok := s.sessions[c.Value]; ok {
			return sb, nil
		}
	}
	if len(s.sessions) >= s.opts.MaxSessions {
		return nil, errors.New("the playground is very busy right now, please try again in a few minutes")
	}
	id := newID()
	st := store.New()
	sb := &sandbox{st: st, eng: engine.New(st), lastUsed: time.Now()}
	s.sessions[id] = sb
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: id, Path: "/api/", HttpOnly: true,
		Secure: s.opts.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: int(s.opts.SessionTTL.Seconds()),
	})
	return sb, nil
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// clientIP is the address used for rate limiting.
func (s *Server) clientIP(r *http.Request) string {
	if s.opts.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			parts := strings.Split(fwd, ",")
			return strings.TrimSpace(parts[len(parts)-1]) // the address our own proxy saw
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type execRequest struct {
	Command string `json:"command"`
}

type execReply struct {
	Reply  string `json:"reply"`
	Micros int64  `json:"micros"`
	Keys   int    `json:"keys"`
	Bytes  int64  `json:"bytes"`
	Error  string `json:"error,omitempty"`
}

// MaxCommand is the longest command the playground accepts.
const MaxCommand = 4096

// maxReply caps what is sent back, so KEYS on a full sandbox stays small.
const maxReply = 64 << 10

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if !s.execLimit.allow(s.clientIP(r), time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, execReply{Error: "slow down a little: too many commands per second"})
		return
	}
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, execReply{Error: "send JSON like {\"command\": \"SET a 1\"}"})
		return
	}
	cmd := strings.TrimSpace(req.Command)
	switch {
	case cmd == "":
		writeJSON(w, http.StatusBadRequest, execReply{Error: "command is empty"})
		return
	case len(cmd) > MaxCommand:
		writeJSON(w, http.StatusBadRequest, execReply{Error: "command is longer than 4 KB"})
		return
	case strings.ContainsAny(cmd, "\r\n"):
		writeJSON(w, http.StatusBadRequest, execReply{Error: "one command at a time, please"})
		return
	}
	sb, err := s.sandboxFor(w, r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, execReply{Error: err.Error()})
		return
	}

	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.lastUsed = time.Now()
	start := time.Now()
	reply := sb.eng.ExecuteLine(cmd)
	micros := time.Since(start).Microseconds()
	s.commands.Add(1)

	st := sb.st.Stats()
	if st.LiveBytes > s.opts.MaxDataBytes {
		sb.st.Clear()
		sb.eng = engine.New(sb.st)
		reply = fmt.Sprintf("(error) your sandbox went over its %d KB limit, so it was reset", s.opts.MaxDataBytes>>10)
		st = sb.st.Stats()
	}
	if len(reply) > maxReply {
		reply = reply[:maxReply] + "\n... (reply cut short)"
	}
	writeJSON(w, http.StatusOK, execReply{Reply: reply, Micros: micros, Keys: st.Keys, Bytes: st.LiveBytes})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if !s.execLimit.allow(s.clientIP(r), time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, execReply{Error: "slow down a little"})
		return
	}
	sb, err := s.sandboxFor(w, r)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, execReply{Error: err.Error()})
		return
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.st.Clear()
	sb.eng = engine.New(sb.st)
	writeJSON(w, http.StatusOK, execReply{Reply: "OK, your sandbox is empty"})
}

type statsReply struct {
	UptimeSeconds  int64 `json:"uptimeSeconds"`
	Sandboxes      int   `json:"sandboxes"`
	CommandsServed int64 `json:"commandsServed"`
	AskEnabled     bool  `json:"askEnabled"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, statsReply{
		UptimeSeconds:  int64(time.Since(s.started).Seconds()),
		Sandboxes:      n,
		CommandsServed: s.commands.Load(),
		AskEnabled:     s.opts.RAG != nil,
	})
}

type askRequest struct {
	Question string `json:"question"`
}

type askSource struct {
	N    int    `json:"n"`
	File string `json:"file"`
	Text string `json:"text"`
}

type askReply struct {
	Answer  string      `json:"answer"`
	Sources []askSource `json:"sources"`
	Error   string      `json:"error,omitempty"`
}

func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	if s.opts.RAG == nil {
		writeJSON(w, http.StatusNotFound, askReply{Error: "the docs assistant is turned off on this server"})
		return
	}
	if !s.askLimit.allow(s.clientIP(r), time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, askReply{Error: "please wait a few seconds between questions"})
		return
	}
	var req askRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		writeJSON(w, http.StatusBadRequest, askReply{Error: "send JSON like {\"question\": \"How does compaction work?\"}"})
		return
	}
	if len(req.Question) > 500 {
		writeJSON(w, http.StatusBadRequest, askReply{Error: "questions are limited to 500 characters"})
		return
	}
	if !s.takeDailyAsk(time.Now()) {
		writeJSON(w, http.StatusTooManyRequests, askReply{Error: "the docs assistant has answered its daily limit of questions; it resets at midnight UTC"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	ans, err := s.opts.RAG.Ask(ctx, req.Question, 4, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, askReply{Error: "the AI model could not answer right now"})
		return
	}
	out := askReply{Answer: ans.Text}
	for _, src := range ans.Sources {
		out.Sources = append(out.Sources, askSource{N: src.N, File: src.File, Text: src.Text})
	}
	writeJSON(w, http.StatusOK, out)
}

// takeDailyAsk counts /api/ask calls per UTC day against AskPerDay.
func (s *Server) takeDailyAsk(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	if day != s.askDay {
		s.askDay, s.askCount = day, 0
	}
	if s.askCount >= s.opts.AskPerDay {
		return false
	}
	s.askCount++
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// staticHandler serves the built site with long caching for hashed assets.
func staticHandler(files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		// Pretty URLs: /privacy serves privacy.html.
		if p := strings.TrimPrefix(r.URL.Path, "/"); p != "" && !strings.Contains(p, ".") {
			if _, err := fs.Stat(files, p+".html"); err == nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/" + p + ".html"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// limiter is a token bucket per visitor IP: each IP gets `burst` tokens,
// refilled at `rate` per second, and each request spends one.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets buckets that have been full for a while, so the map
// doesn't grow forever.
func (l *limiter) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.buckets, k)
		}
	}
}
