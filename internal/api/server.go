// Package api is the HTTP layer: a streaming chat endpoint (SSE) that runs
// the agent, a plain search endpoint, health, and the built web UI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

// Catalog is what the HTTP layer needs from the store.
type Catalog interface {
	agent.Catalog
	Ping(ctx context.Context) error
	Count(ctx context.Context) (int, error)
}

// Runner answers chat questions (implemented by *agent.Agent).
type Runner interface {
	Run(ctx context.Context, history []agent.Message, emit func(agent.Event)) (*agent.Result, error)
	Config() agent.Config
}

// Server wires the handlers together.
type Server struct {
	Catalog     Catalog
	Agent       Runner // nil when no Anthropic credentials are configured
	PreviewRows bool
	WebDir      string // optional: directory with the built frontend
	Logger      *slog.Logger

	// ChatPerMinute limits chat requests per client IP (0 = unlimited).
	ChatPerMinute int
	// ChatPerDay caps chat requests across all clients per UTC day
	// (0 = unlimited), so a public demo has a bounded API bill.
	ChatPerDay int

	limiterOnce sync.Once
	limiters    sync.Map // ip -> *rate.Limiter

	dayMu    sync.Mutex
	day      string
	dayCount int
}

const (
	maxHistory     = 20
	maxMessageLen  = 4000
	maxRequestBody = 128 << 10
)

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/categories", s.categories)
	mux.HandleFunc("POST /api/chat", s.chat)
	if s.WebDir != "" {
		mux.Handle("/", spaHandler(s.WebDir))
	}
	return s.logRequests(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	body := map[string]any{"status": "ok", "preview_rows": s.PreviewRows}
	status := http.StatusOK
	if err := s.Catalog.Ping(ctx); err != nil {
		body["status"], body["db"] = "degraded", err.Error()
		status = http.StatusServiceUnavailable
	} else if n, err := s.Catalog.Count(ctx); err == nil {
		body["db"], body["datasets"] = "ok", n
	}
	if s.Agent != nil {
		cfg := s.Agent.Config()
		info := map[string]any{"enabled": true, "provider": cfg.Provider, "model": cfg.Model, "effort": cfg.Effort}
		if cfg.Provider == agent.ProviderAnthropic {
			info["fallbacks"] = cfg.Fallbacks
		}
		body["agent"] = info
	} else {
		body["agent"] = map[string]any{"enabled": false, "reason": "no LLM credentials on the server; search-only mode"}
	}
	writeJSON(w, status, body)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	hits, err := s.Catalog.Search(r.Context(), store.SearchParams{
		Query: q.Get("q"), Category: q.Get("category"), Agency: q.Get("agency"), Limit: limit,
	})
	if errors.Is(err, store.ErrEmptyQuery) {
		writeError(w, http.StatusBadRequest, "q, category or agency is required")
		return
	}
	if err != nil {
		s.Logger.Error("search failed", "err", err)
		writeError(w, http.StatusInternalServerError, "search failed")
		return
	}
	type result struct {
		store.SearchHit
		URL string `json:"url"`
	}
	out := make([]result, len(hits))
	for i, h := range hits {
		out[i] = result{h, h.URL()}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "results": out})
}

func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	cats, err := s.Catalog.Categories(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "categories unavailable")
		return
	}
	writeJSON(w, http.StatusOK, cats)
}

type chatRequest struct {
	Messages []agent.Message `json:"messages"`
}

// chat streams agent events as Server-Sent Events:
//
//	event: tool_call
//	data: {"type":"tool_call","id":"toolu_…","name":"search_datasets","input":{…}}
//
// The stream always ends with a "done" or "error" event.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		writeError(w, http.StatusServiceUnavailable, "the AI agent is disabled on this server (no LLM credentials); use /api/search")
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateHistory(req.Messages); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Rate-limit after validation so malformed requests don't burn quota.
	if !s.allow(r) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many questions, please wait a minute")
		return
	}
	if !s.allowToday() {
		writeError(w, http.StatusTooManyRequests, "the demo's daily question budget is used up; try again tomorrow or run it locally")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	w.WriteHeader(http.StatusOK)

	var mu sync.Mutex
	send := func(ev agent.Event) {
		data, _ := json.Marshal(ev)
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		flusher.Flush()
	}

	// Heartbeat comments keep proxies from closing an idle stream while
	// Claude is thinking.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				mu.Lock()
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			}
		}
	}()

	res, err := s.Agent.Run(ctx, req.Messages, send)
	switch {
	case errors.Is(err, agent.ErrRefused):
		send(agent.Event{Type: "done", Done: res})
	case errors.Is(err, agent.ErrQuota):
		s.Logger.Warn("LLM quota or rate limit reached", "err", err)
		send(agent.Event{Type: "error", Code: "llm_quota",
			Text: "The AI model's quota is used up for now (the free tier allows a limited number of questions per day), so here are plain search results instead."})
	case err != nil:
		s.Logger.Error("agent run failed", "err", err)
		send(agent.Event{Type: "error", Text: userFacingError(err)})
	default:
		s.Logger.Info("chat answered", "turns", res.Turns, "tool_calls", len(res.ToolCalls),
			"citations", len(res.Citations), "input_tokens", res.Usage.InputTokens,
			"output_tokens", res.Usage.OutputTokens, "ms", res.DurationMS)
		send(agent.Event{Type: "done", Done: res})
	}
}

func validateHistory(msgs []agent.Message) error {
	if len(msgs) == 0 {
		return errors.New("messages must not be empty")
	}
	if len(msgs) > maxHistory {
		return fmt.Errorf("at most %d messages per conversation", maxHistory)
	}
	for i, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("message %d: role must be user or assistant", i)
		}
		if strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("message %d is empty", i)
		}
		if len([]rune(m.Content)) > maxMessageLen {
			return fmt.Errorf("message %d is longer than %d characters", i, maxMessageLen)
		}
		if i > 0 && msgs[i-1].Role == m.Role {
			return errors.New("messages must alternate between user and assistant")
		}
	}
	if msgs[0].Role != "user" || msgs[len(msgs)-1].Role != "user" {
		return errors.New("the conversation must start and end with a user message")
	}
	return nil
}

func userFacingError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "The agent took too long to answer. Please try a more specific question."
	}
	if errors.Is(err, context.Canceled) {
		return "Request cancelled."
	}
	return "The agent failed to answer: " + err.Error()
}

func (s *Server) allow(r *http.Request) bool {
	if s.ChatPerMinute <= 0 {
		return true
	}
	ip := clientIP(r)
	l, _ := s.limiters.LoadOrStore(ip, rate.NewLimiter(rate.Every(time.Minute/time.Duration(s.ChatPerMinute)), max(1, s.ChatPerMinute/2)))
	s.limiterOnce.Do(func() { go s.forgetLimiters() })
	return l.(*rate.Limiter).Allow()
}

func (s *Server) allowToday() bool {
	if s.ChatPerDay <= 0 {
		return true
	}
	s.dayMu.Lock()
	defer s.dayMu.Unlock()
	if today := time.Now().UTC().Format(time.DateOnly); today != s.day {
		s.day, s.dayCount = today, 0
	}
	if s.dayCount >= s.ChatPerDay {
		return false
	}
	s.dayCount++
	return true
}

// forgetLimiters drops per-IP state every hour so the map cannot grow forever.
func (s *Server) forgetLimiters() {
	for range time.Tick(time.Hour) {
		s.limiters.Clear()
	}
}

func clientIP(r *http.Request) string {
	// Behind the bundled nginx / a PaaS router the client is the first hop.
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// spaHandler serves the built frontend and falls back to index.html for
// client-side routes.
func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			if r.URL.Path != "/" && !strings.HasSuffix(r.URL.Path, "/") {
				http.ServeFile(w, r, filepath.Join(dir, "index.html"))
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.Logger.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
