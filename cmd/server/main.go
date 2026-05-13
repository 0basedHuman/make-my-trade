// cmd/server/main.go
//
// WHAT: Entry point for the MakeMyTrade HTTP server.
//       Serves the trading dashboard UI and REST API.
//
// WHY:  This process owns the web interface and the run-analysis pipeline.
//       The Temporal worker (cmd/worker) owns scheduled workflows.
//       Both run migrations on startup — idempotent and safe.
//
// HOW:
//   1. Load config → run migrations → open DB pool → open Redis
//   2. Register HTTP routes:
//       GET  /         → serve dashboard UI (embedded from web/static/)
//       GET  /health   → liveness check
//       GET  /api/daily-analysis    → latest analysis results from DB
//       POST /api/run-analysis      → trigger fresh pipeline run
//       POST /api/run-confirmation  → evaluate opening 10-min bars, promote entry_ready → confirmed
//       GET  /api/paper-positions   → open paper positions
//   3. Serve with graceful shutdown on SIGINT/SIGTERM
//
// WHAT BREAKS: If web/static/index.html is missing at compile time the
//              embed will fail — run from project root.
//
// VERIFY: http://localhost:8080 → dashboard
//         http://localhost:8080/health → {"status":"ok","db":"ok","redis":"ok"}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourname/makemytrade/config"
	"github.com/yourname/makemytrade/internal/api"
	"github.com/yourname/makemytrade/internal/store"
	"github.com/yourname/makemytrade/web"
)

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg := config.Load()
	log.Printf("config: loaded (env=%s, port=%d)", cfg.Env, cfg.Port)

	// ── 2. Migrations ─────────────────────────────────────────────────────────
	migrationsPath, err := filepath.Abs("migrations")
	if err != nil {
		log.Fatalf("migrations: path: %v", err)
	}
	if _, err := os.Stat(migrationsPath); os.IsNotExist(err) {
		log.Fatalf("migrations: directory not found at %s — run from project root", migrationsPath)
	}
	if err := store.RunMigrations(cfg.DBURL, migrationsPath); err != nil {
		log.Fatalf("migrations: %v", err)
	}

	// ── 3. Postgres ───────────────────────────────────────────────────────────
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("db: pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("db: ping: %v", err)
	}
	log.Println("db: connection pool ready")

	// ── 4. Redis ──────────────────────────────────────────────────────────────
	redisOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("redis: invalid URL: %v", err)
	}
	rdb := redis.NewClient(redisOpt)
	defer rdb.Close()
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		log.Fatalf("redis: ping: %v", err)
	}
	log.Println("redis: PONG")

	// ── 5. HTTP routes ────────────────────────────────────────────────────────
	h := api.New(pool, cfg)
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("/health", healthHandler(pool, rdb))

	// API
	mux.HandleFunc("/api/run-analysis", h.RunAnalysis)
	mux.HandleFunc("/api/run-confirmation", h.RunConfirmation)
	mux.HandleFunc("/api/run-position-review", h.RunPositionReview)
	mux.HandleFunc("/api/force-confirm", h.ForceConfirm)
	mux.HandleFunc("/api/daily-analysis", h.DailyAnalysis)
	mux.HandleFunc("/api/paper-positions", h.PaperPositions)
	mux.HandleFunc("/api/rejection-analytics", h.RejectionAnalytics)

	// Static UI — serve from embedded FS (web/embed.go)
	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		log.Fatalf("static: embed sub: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/", fileServer)

	// ── 6. HTTP server with graceful shutdown ──────────────────────────────────
	shutdownCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 8 * time.Minute, // analysis pipeline: signals + chains + Claude
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("server: listening on http://localhost:%d", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	<-shutdownCtx.Done()
	log.Println("server: shutting down…")

	shutdownTO, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownTO); err != nil {
		log.Printf("server: forced shutdown: %v", err)
	}
	log.Println("server: stopped")
}

func healthHandler(pool *pgxpool.Pool, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		status := map[string]string{"status": "ok", "db": "ok", "redis": "ok"}
		httpStatus := http.StatusOK

		if err := pool.Ping(ctx); err != nil {
			status["status"] = "degraded"
			status["db"] = fmt.Sprintf("error: %v", err)
			httpStatus = http.StatusServiceUnavailable
		}
		if _, err := rdb.Ping(ctx).Result(); err != nil {
			status["status"] = "degraded"
			status["redis"] = fmt.Sprintf("error: %v", err)
			httpStatus = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		json.NewEncoder(w).Encode(status)
	}
}
