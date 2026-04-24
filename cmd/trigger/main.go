package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	wf "github.com/yourname/makemytrade/internal/workflow"
	"go.temporal.io/sdk/client"
)

func main() {
	host := os.Getenv("TEMPORAL_HOST")
	if host == "" {
		host = "localhost:7233"
	}
	c, err := client.Dial(client.Options{HostPort: host})
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer c.Close()

	id := fmt.Sprintf("mechanical-risk-test-%d", time.Now().Unix())
	run, err := c.ExecuteWorkflow(context.Background(),
		client.StartWorkflowOptions{
			ID:                 id,
			TaskQueue:          "makemytrade-main",
			WorkflowRunTimeout: 30 * time.Second,
		},
		wf.MechanicalRiskCycle,
	)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	fmt.Printf("started workflow_id=%s run_id=%s\n", id, run.GetRunID())

	var result string
	if err := run.Get(context.Background(), &result); err != nil {
		log.Printf("workflow finished with: %v", err)
	} else {
		fmt.Printf("result: %q\n", result)
	}
}
