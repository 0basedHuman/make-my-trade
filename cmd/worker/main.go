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
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourname/makemytrade/config"
	"github.com/yourname/makemytrade/internal/market"
	"github.com/yourname/makemytrade/internal/store"
	"github.com/yourname/makemytrade/internal/strategy"
	wf "github.com/yourname/makemytrade/internal/workflow"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
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
	w.RegisterWorkflow(wf.FirstPositionReviewCycle)
	w.RegisterWorkflow(wf.ContinuationReviewCycle)
	w.RegisterWorkflow(wf.DailyPositionReview)
	w.RegisterWorkflow(wf.WeeklyReviewCycle)

	// Register activities (bound to deps so they have DB + API access)
	w.RegisterActivity(deps.RunDailyAnalysisActivity)
	w.RegisterActivity(deps.RunOpeningConfirmationActivity)
	w.RegisterActivity(deps.RunPositionReviewActivity)
	w.RegisterActivity(deps.RunContinuationReviewActivity)
	w.RegisterActivity(deps.RunWeeklyReviewActivity)

	// ── 8. Start ──────────────────────────────────────────────────────────────
	if err := w.Start(); err != nil {
		log.Fatalf("worker: start: %v", err)
	}
	log.Printf("worker: started on task queue %q", TaskQueue)
	log.Println("worker: polling — press Ctrl+C to stop")

	// ── 9. Register Temporal schedules (idempotent — skip if already exist) ──
	registerSchedules(ctx, temporalClient, workerRules)

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)
	<-shutdownCh

	log.Println("worker: draining in-flight activities…")
	w.Stop()
	log.Println("worker: stopped")
}

// registerSchedules creates the six autonomous Temporal schedules.
//
// All cron expressions are in America/Los_Angeles PT-local time.
// Temporal handles DST automatically via TimeZoneName — no UTC conversion needed.
//
// Times are read from strategy_rules.yaml schedule: block. If the YAML value is
// missing or unparseable, the hardcoded default is used and a warning is logged.
//
//	DailyResearchCycle         06:25 PT  weekdays  — overnight scan
//	OpeningConfirmationCycle   06:42 PT  weekdays  — first-10-min entry (stale guard at 06:55)
//	FirstPositionReviewCycle   07:15 PT  weekdays  — early risk management
//	ContinuationReviewCycle    07:45 PT  weekdays  — fresh intraday review
//	DailyPositionReview        12:45 PT  weekdays  — end-of-day: hold vs exit
//	WeeklyReviewCycle          07:00 PT  Sunday    — performance + tuning proposals
//
// Each call is idempotent: if a schedule already exists the error is logged and
// the worker continues — the existing schedule is not touched.
//
// TODO: to change a schedule time, update strategy_rules.yaml schedule: block,
// delete the old Temporal schedule (Temporal UI or tctl), then restart the worker.
func registerSchedules(ctx context.Context, tc client.Client, rules *strategy.Rules) {
	const tz = "America/Los_Angeles"

	// Helper: parse "HH:MM" from YAML or fall back to default "HH:MM".
	parseCron := func(yamlTime, defaultTime, weekdays string) string {
		t := defaultTime
		if yamlTime != "" {
			t = yamlTime
		}
		var h, m int
		if _, err := fmt.Sscanf(t, "%d:%d", &h, &m); err != nil {
			log.Printf("worker: schedule: could not parse %q — using default %s", t, defaultTime)
			fmt.Sscanf(defaultTime, "%d:%d", &h, &m) //nolint:errcheck
		}
		return fmt.Sprintf("%d %d * * %s", m, h, weekdays)
	}

	sched := rules.Schedule
	type schedSpec struct {
		id       string
		cron     string
		workflow interface{}
		wfID     string
	}
	schedules := []schedSpec{
		{
			id:       "makemytrade-daily-research",
			cron:     parseCron(sched.DailyScanTime, "06:25", "1-5"),
			workflow: wf.DailyResearchCycle,
			wfID:     "daily-research-run",
		},
		{
			id:       "makemytrade-open-confirmation",
			cron:     parseCron(sched.OpeningConfirmationTime, "06:42", "1-5"),
			workflow: wf.OpeningConfirmationCycle,
			wfID:     "open-confirmation-run",
		},
		{
			id:       "makemytrade-first-position-review",
			cron:     parseCron(sched.FirstPositionReviewTime, "07:15", "1-5"),
			workflow: wf.FirstPositionReviewCycle,
			wfID:     "first-position-review-run",
		},
		{
			id:       "makemytrade-continuation-review",
			cron:     parseCron(sched.ContinuationReviewTime, "07:45", "1-5"),
			workflow: wf.ContinuationReviewCycle,
			wfID:     "continuation-review-run",
		},
		{
			id:       "makemytrade-position-review",
			cron:     parseCron(sched.EndOfDayReviewTime, "12:45", "1-5"),
			workflow: wf.DailyPositionReview,
			wfID:     "position-review-run",
		},
		{
			id:       "makemytrade-weekly-review",
			cron:     parseCron(sched.WeeklyReviewTime, "07:00", "0"),
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
				TimeZoneName:    tz,
				Jitter:          20 * time.Second,
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
			log.Printf("worker: schedule %q registered (%s %s)", s.id, s.cron, tz)
		}
	}
}
