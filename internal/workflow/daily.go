// internal/workflow/daily.go
//
// WHAT: Temporal workflow definitions for scheduled daily analysis.
//
// WHY:  Temporal makes the daily pipeline fault-tolerant — if the server
//       restarts mid-run, Temporal replays from the last completed activity
//       rather than starting over. It also handles retry logic automatically.
//
// HOW:  DailyResearchCycle is the main workflow. It is scheduled via a
//       Temporal cron at "30 6 * * 1-5" (6:30 AM PT weekdays).
//       It calls activities defined in activities.go.
//       On Day 0, the HTTP handler (api/handlers.go) runs the pipeline
//       directly. Temporal workflows kick in from Day 1 onward.
//
// SCHEDULE: 6:30 AM Pacific Time = "30 13 * * 1-5" in UTC (PST)
//           or "30 14 * * 1-5" during PDT (daylight saving).
//           The Temporal scheduler should use America/Los_Angeles timezone.
//
// WHAT BREAKS: If the task queue name in the workflow schedule doesn't match
//              TaskQueue in cmd/worker/main.go, the workflow never executes.
//
// VERIFY: After `make dev`, open Temporal UI at http://localhost:8088
//         and check Workflows for DailyResearchCycle.

package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// DailyResearchCycle is the main daily workflow.
// Runs at 6:30 AM PT weekdays. Fetches data, runs analysis, calls Claude.
func DailyResearchCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("DailyResearchCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:        3,
			InitialInterval:        30 * time.Second,
			BackoffCoefficient:     2.0,
			MaximumInterval:        5 * time.Minute,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Step 1: Run the full analysis pipeline
	// Activity is registered as method on ActivityDeps — reference by name string.
	var result string
	if err := workflow.ExecuteActivity(ctx, "RunDailyAnalysisActivity").Get(ctx, &result); err != nil {
		logger.Error("DailyResearchCycle: RunDailyAnalysis failed", "error", err)
		return err
	}

	logger.Info("DailyResearchCycle: complete", "result", result)
	return nil
}

// OpeningConfirmationCycle runs at 6:40 AM PT to check the first 10-minute candle.
// It updates trade_confirmations for any candidates from this morning.
func OpeningConfirmationCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("OpeningConfirmationCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 15 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunOpeningConfirmationActivity").Get(ctx, &result); err != nil {
		logger.Error("OpeningConfirmationCycle: failed", "error", err)
		return err
	}

	logger.Info("OpeningConfirmationCycle: complete", "result", result)
	return nil
}

// WeeklyReviewCycle runs once per week (Sunday morning) to generate the
// weekly paper-trade performance review and strategy tuning proposals.
func WeeklyReviewCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("WeeklyReviewCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 60 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunWeeklyReviewActivity").Get(ctx, &result); err != nil {
		logger.Error("WeeklyReviewCycle: failed", "error", err)
		return err
	}

	logger.Info("WeeklyReviewCycle: complete", "result", result)
	return nil
}

// DailyPositionReview runs at 3:45 PM PT (before close) to review held positions.
func DailyPositionReview(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("DailyPositionReview: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 30 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunPositionReviewActivity").Get(ctx, &result); err != nil {
		logger.Error("DailyPositionReview: failed", "error", err)
		return err
	}

	logger.Info("DailyPositionReview: complete", "result", result)
	return nil
}
