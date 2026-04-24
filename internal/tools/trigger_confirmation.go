//go:build ignore

package main

import (
	"context"
	"fmt"
	"log"

	"go.temporal.io/sdk/client"
	wf "github.com/yourname/makemytrade/internal/workflow"
)

func main() {
	tc, err := client.Dial(client.Options{HostPort: "localhost:7233", Namespace: "default"})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer tc.Close()

	run, err := tc.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "manual-open-confirmation-2026-04-24",
		TaskQueue: "makemytrade-main",
	}, wf.OpeningConfirmationCycle)
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}
	fmt.Printf("workflow started: runID=%s\n", run.GetRunID())
	fmt.Println("waiting for completion...")
	if err := run.Get(context.Background(), nil); err != nil {
		log.Fatalf("workflow error: %v", err)
	}
	fmt.Println("OpeningConfirmationCycle complete — check paper_positions and worker logs")
}
