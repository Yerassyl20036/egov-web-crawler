// Command api serves the chat agent, the search API and (optionally) the
// built web UI.
//
// Environment:
//
//	DATABASE_URL        Postgres connection string (required)
//	PORT                listen port (default 8080)
//	ANTHROPIC_API_KEY   enables the AI agent; without it the app runs search-only
//	AGENT_MODEL         default claude-opus-5-5 (see internal/agent for the rest)
//	EGOV_API_KEY        enables the preview_rows tool (data.egov.kz API key)
//	WEB_DIR             serve the built frontend from this directory
//	AUTO_SEED_FILE      seed this snapshot on start when the catalog is empty
//	CHAT_RATE_PER_MIN   chat requests per client IP per minute (default 10)
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Yerassyl20036/egov-web-crawler/internal/agent"
	"github.com/Yerassyl20036/egov-web-crawler/internal/api"
	"github.com/Yerassyl20036/egov-web-crawler/internal/egov"
	"github.com/Yerassyl20036/egov-web-crawler/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}
	db, err := store.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	if file := os.Getenv("AUTO_SEED_FILE"); file != "" {
		if n, err := db.Count(ctx); err == nil && n == 0 {
			read, err := db.SeedFile(ctx, file)
			if err != nil {
				return err
			}
			logger.Info("seeded empty catalog", "file", file, "datasets", read)
		}
	}

	tools := &agent.Tools{Catalog: db}
	if key := os.Getenv("EGOV_API_KEY"); key != "" {
		tools.Rows = egov.NewClient(key)
	}

	srv := &api.Server{
		Catalog:       db,
		PreviewRows:   tools.Rows != nil,
		WebDir:        os.Getenv("WEB_DIR"),
		Logger:        logger,
		ChatPerMinute: envInt("CHAT_RATE_PER_MIN", 10),
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		cfg := agent.FromEnv()
		srv.Agent = agent.New(cfg, tools)
		logger.Info("agent enabled", "model", cfg.Model, "effort", cfg.Effort, "fallbacks", cfg.Fallbacks, "preview_rows", srv.PreviewRows)
	} else {
		logger.Warn("ANTHROPIC_API_KEY not set: running in search-only mode")
	}

	httpServer := &http.Server{
		Addr:              ":" + envOr("PORT", "8080"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return fallback
}
