// cmd/worker/main.go
//
// WHAT: Entry point for the Temporal worker process.
//       Registers all workflows and activities, then polls the task queue.
//
// WHY:  The worker is separate from the HTTP server so they can be
//       scaled independently and fail independently.
//
// HOW:  At startup it:
//         1. Loads config + runs migrations
//         2. Opens DB pool + Redis
//         3. Connects to Temporal server
//         4. Registers DailyResearchCycle + activities
//         5. Polls "makemytrade-main" indefinitely until SIGINT
//
// WHAT BREAKS: If the task queue name doesn't match the workflow schedule,
//              workflows queue up in Temporal forever with no worker to run them.
//              Task queue: "makemytrade-main" — must match everywhere.
//
// VERIFY: make dev → [worker] worker: started on task queue "makemytrade-main"
//         Temporal UI at http://localhost:8088 shows worker connected.

package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"github.com/yourname/makemytrade/config"
	"github.com/yourname/makemytrade/internal/market"
	"github.com/yourname/makemytrade/internal/store"
	"github.com/yourname/makemytrade/internal/strategy"
	wf "github.com/yourname/makemytrade/internal/workflow"
)

const TaskQueue = "makemytrade-main"

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg := config.Load()
	log.Printf("config: loaded (env=%s)", cfg.Env)

	// ── 2. Migrations ─────────────────────────────────────────────────────────
	migrationsPath, err := filepath.Abs("migrations")
	if err != nil {
		log.Fatalf("migrations: path: %v", err)
	}
	if _, err := os.Stat(migrationsPath); os.IsNotExist(err) {
		log.Fatalf("migrations: not found at %s — run from project root", migrationsPath)
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
		log.Fatalf("redis: URL: %v", err)
	}
	rdb := redis.NewClient(redisOpt)
	defer rdb.Close()
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		log.Fatalf("redis: ping: %v", err)
	}
	log.Println("redis: PONG")

	// ── 5. Temporal client ────────────────────────────────────────────────────
	temporalClient, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHost,
		Namespace: cfg.TemporalNamespace,
	})
	if err != nil {
		log.Fatalf("temporal: connect %s: %v", cfg.TemporalHost, err)
	}
	defer temporalClient.Close()
	log.Printf("temporal: connected to %s", cfg.TemporalHost)

	// ── 6. Activity dependencies ──────────────────────────────────────────────
	workerRules, rulesErr := strategy.LoadRules("strategy_rules.yaml")
	if rulesErr != nil {
		log.Printf("worker: warning — could not load strategy_rules.yaml: %v — using defaults", rulesErr)
		workerRules = strategy.DefaultRules()
	}
	deps := &wf.ActivityDeps{
		Pool:    pool,
		Cfg:     cfg,
		Alpaca:  market.NewAlpacaClient(cfg.AlpacaPaperAPIKey, cfg.AlpacaPaperSecret, cfg.AlpacaDataURL, cfg.AlpacaBaseURL),
		Finnhub: market.NewFinnhubClient(cfg.FinnhubAPIKey),
		FRED:    market.NewFREDClient(cfg.FREDAPIKey),
		Engine:  strategy.NewEngine(strategy.DefaultConfig(), workerRules),
		Rules:   workerRules,
	}

	// ── 7. Temporal worker ────────────────────────────────────────────────────
	w := worker.New(temporalClient, TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     4,
		MaxConcurrentWorkflowTaskExecutionSize: 4,
	})

	// Register workflows
	w.RegisterWorkflow(wf.DailyResearchCycle)
	w.RegisterWorkflow(wf.OpeningConfirmationCycle)
	w.RegisterWorkflow(wf.DailyPositionReview)
	w.RegisterWorkflow(wf.WeeklyReviewCycle)

	// Register activities (bound to deps so they have DB + API access)
	w.RegisterActivity(deps.RunDailyAnalysisActivity)
	w.RegisterActivity(deps.RunOpeningConfirmationActivity)
	w.RegisterActivity(deps.RunPositionReviewActivity)
	w.RegisterActivity(deps.RunWeeklyReviewActivity)

	// ── 8. Start ──────────────────────────────────────────────────────────────
	if err := w.Start(); err != nil {
		log.Fatalf("worker: start: %v", err)
	}
	log.Printf("worker: started on task queue %q", TaskQueue)
	log.Println("worker: polling — press Ctrl+C to stop")

	// ── 9. Register Temporal schedules (idempotent — skip if already exist) ──
	registerSchedules(ctx, temporalClient)

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownCh

	log.Println("worker: draining in-flight activities…")
	w.Stop()
	log.Println("worker: stopped")
}

// registerSchedules creates the four autonomous Temporal schedules.
// All times are UTC; they map to America/Los_Angeles as:
//
//	DailyResearchCycle:        14:00 UTC = 6:00 AM PST (weekdays)
//	OpeningConfirmationCycle:  14:45 UTC = 6:45 AM PST (weekdays, after 10-min open)
//	DailyPositionReview:       20:45 UTC = 12:45 PM PST (weekdays, before 1 PM PT close)
//	WeeklyReviewCycle:         15:00 UTC = 7:00 AM PST (Sunday)
//
// Each call is idempotent: if a schedule already exists the error is logged and
// the worker continues — the existing schedule is not touched.
func registerSchedules(ctx context.Context, tc client.Client) {
	type schedSpec struct {
		id       string
		cron     string
		workflow interface{}
		wfID     string
	}
	schedules := []schedSpec{
		{
			id:       "makemytrade-daily-research",
			cron:     "0 14 * * 1-5",
			workflow: wf.DailyResearchCycle,
			wfID:     "daily-research-run",
		},
		{
			id:       "makemytrade-open-confirmation",
			cron:     "45 14 * * 1-5",
			workflow: wf.OpeningConfirmationCycle,
			wfID:     "open-confirmation-run",
		},
		{
			id:       "makemytrade-position-review",
			cron:     "45 20 * * 1-5",
			workflow: wf.DailyPositionReview,
			wfID:     "position-review-run",
		},
		{
			id:       "makemytrade-weekly-review",
			cron:     "0 15 * * 0",
			workflow: wf.WeeklyReviewCycle,
			wfID:     "weekly-review-run",
		},
	}

	sc := tc.ScheduleClient()
	for _, s := range schedules {
		_, err := sc.Create(ctx, client.ScheduleOptions{
			ID: s.id,
			Spec: client.ScheduleSpec{
				CronExpressions: []string{s.cron},
				Jitter:          30 * time.Second, // spread load slightly
			},
			Action: &client.ScheduleWorkflowAction{
				ID:        s.wfID,
				Workflow:  s.workflow,
				TaskQueue: TaskQueue,
			},
		})
		if err != nil {
			// "already exists" is expected on restarts — not fatal
			log.Printf("worker: schedule %q: %v", s.id, err)
		} else {
			log.Printf("worker: schedule %q registered (%s UTC)", s.id, s.cron)
		}
	}
}
