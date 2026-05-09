// internal/workflow/daily.go
//
// WHAT: Temporal workflow definitions for the autonomous paper-trading day.
//
// WHY:  Temporal makes the daily pipeline fault-tolerant — if the server
//       restarts mid-run, Temporal replays from the last completed activity
//       rather than starting over. It also handles retry logic automatically.
//
// HOW:  Five workflows cover the full trading day. All are scheduled in
//       cmd/worker/main.go using TimeZoneName="America/Los_Angeles" so the
//       cron expressions are PT-local and DST is handled automatically.
//
// AUTONOMOUS TRADING DAY (America/Los_Angeles):
//
//   06:25  DailyResearchCycle         — overnight scan, classify candidates
//   06:42  OpeningConfirmationCycle   — first 10-min candle, deterministic confirmation
//   every 10m MechanicalRiskCycle    — stop/TP/trail/time-stop checks
//   12:45  DailyPositionReview        — end-of-day: log overnight holds
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
// Runs at 5:30 AM PT weekdays (must finish before 6:30 AM open).
//
// Recovery path: after analysis completes, immediately attempts opening
// confirmation as a non-fatal step. On normal days the too-early guard
// (before 6:40 AM) causes it to return cleanly and the scheduled
// OpeningConfirmationCycle at 6:42 handles it. On late-start days
// (worker was offline, Temporal replayed the missed schedule), this
// step runs confirmation right after research within the 8:30 cutoff.
func DailyResearchCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("DailyResearchCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    30 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    5 * time.Minute,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	// Step 1: Run the full analysis pipeline.
	var result string
	if err := workflow.ExecuteActivity(ctx, "RunDailyAnalysisActivity").Get(ctx, &result); err != nil {
		logger.Error("DailyResearchCycle: RunDailyAnalysis failed", "error", err)
		return err
	}
	logger.Info("DailyResearchCycle: analysis complete", "result", result)

	// Step 2: Late-start recovery confirmation.
	// Uses a 5-min timeout and a single attempt — non-fatal.
	confirmAO := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	}
	confirmCtx := workflow.WithActivityOptions(ctx, confirmAO)
	var confirmResult string
	if err := workflow.ExecuteActivity(confirmCtx, "RunOpeningConfirmationActivity").Get(confirmCtx, &confirmResult); err != nil {
		logger.Warn("DailyResearchCycle: late-start confirmation failed (non-fatal)", "error", err)
	} else {
		logger.Info("DailyResearchCycle: late-start confirmation", "result", confirmResult)
	}

	return nil
}

// OpeningConfirmationCycle runs at 6:42 AM PT.
// Uses 6:30–6:40 bars only. Guarded against stale runs (cutoff 6:55 AM PT).
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

// FirstPositionReviewCycle runs at 7:15 AM PT for early risk management.
// Reviews open positions using current option mid-prices. Applies HOLD/EXIT
// decisions before the first hour of trading is complete.
func FirstPositionReviewCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("FirstPositionReviewCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 8 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 20 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunPositionReviewActivity").Get(ctx, &result); err != nil {
		logger.Error("FirstPositionReviewCycle: failed", "error", err)
		return err
	}

	logger.Info("FirstPositionReviewCycle: complete", "result", result)
	return nil
}

// ContinuationReviewCycle runs at 7:45 AM PT.
// Uses fresh intraday bars from 6:30 to ~7:45 AM PT — NOT the stale first-10-min
// opening candle. Reviews open positions for continuation/tighten/exit.
// TODO: add continuation entry logic for still-valid entry_ready setups.
func ContinuationReviewCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("ContinuationReviewCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 30 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunContinuationReviewActivity").Get(ctx, &result); err != nil {
		logger.Error("ContinuationReviewCycle: failed", "error", err)
		return err
	}

	logger.Info("ContinuationReviewCycle: complete", "result", result)
	return nil
}

// DailyPositionReview runs at 12:45 PM PT (before close) for end-of-day decisions.
// Uses RunEODPositionReviewActivity: mechanical checks first, then logs overnight holds.
// 21-45 DTE swings hold overnight by default — no forced exits.
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
	if err := workflow.ExecuteActivity(ctx, "RunEODPositionReviewActivity").Get(ctx, &result); err != nil {
		logger.Error("DailyPositionReview: failed", "error", err)
		return err
	}

	logger.Info("DailyPositionReview: complete", "result", result)
	return nil
}

// MechanicalRiskCycle runs every 10 minutes during market hours (06:50–12:50 PT weekdays).
// Evaluates hard mechanical exit rules (stop, take-profit, trailing, EOD) for every
// open paper position without waiting for Claude.
func MechanicalRiskCycle(ctx workflow.Context) error {
	logger := workflow.GetLogger(ctx)
	logger.Info("MechanicalRiskCycle: starting")

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
			InitialInterval: 15 * time.Second,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result string
	if err := workflow.ExecuteActivity(ctx, "RunMechanicalRiskCheckActivity").Get(ctx, &result); err != nil {
		logger.Error("MechanicalRiskCycle: failed", "error", err)
		return err
	}

	logger.Info("MechanicalRiskCycle: complete", "result", result)
	return nil
}
