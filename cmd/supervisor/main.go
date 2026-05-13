// cmd/supervisor/main.go
//
// WHAT: Single process that owns the entire MakeMyTrade stack.
//       Replaces start.sh + stop.sh.
//
// WHY:  Previously, server and worker were started independently via nohup.
//       Stale go-run workers from old dev sessions stayed alive, competed on
//       the Temporal task queue, and ran old code silently. A supervisor makes
//       the lifecycle explicit: one process to start, one PID to kill, clean
//       shutdown guaranteed.
//
// HOW:
//   1. Load .env → kill orphaned workers → build server + worker
//   2. Start Docker services → wait for Postgres + Temporal
//   3. Start caffeinate (macOS sleep prevention, watches supervisor PID)
//   4. Start server subprocess → wait for /health → start worker subprocess
//   5. Block until SIGTERM or SIGINT
//   6. SIGTERM both children → wait up to 10s → kill if unresponsive → exit
//   7. If a child crashes: restart with exponential backoff, up to 3 times,
//      then exit (something is fundamentally broken, don't spin forever)
//
// USAGE:
//   Build:      go build -o bin/supervisor ./cmd/supervisor
//   Start:      ./bin/supervisor              (foreground; Ctrl-C to stop)
//   Background: nohup ./bin/supervisor > logs/supervisor.log 2>&1 &
//   Stop:       make stop   (or: kill $(cat logs/supervisor.pid))
//   Logs:       tail -f logs/server.log logs/worker.log
//
// WHAT BREAKS: If Docker is not running, waitPort will block forever.
//              If bin/ does not exist, build will create it.

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const (
	supervisorPIDFile = "logs/supervisor.pid"
	serverHealthURL   = "http://localhost:8080/health"
	maxRestarts       = 3
)

func main() {
	log.SetFlags(log.Ltime)

	if err := godotenv.Load(); err != nil {
		log.Printf("warn: .env not loaded (%v)", err)
	}

	os.MkdirAll("bin", 0755)
	os.MkdirAll("logs", 0755)

	writePID(supervisorPIDFile)
	defer os.Remove(supervisorPIDFile)

	log.Println("=== MakeMyTrade supervisor ===")

	killOrphans()
	buildBinaries()
	startDocker()
	waitPort("localhost:5432", "Postgres")
	waitPort("localhost:7233", "Temporal")
	startCaffeinate()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		s := <-ch
		log.Printf("[~] Received %v — shutting down...", s)
		cancel()
	}()

	// Server starts first; worker waits until server is healthy.
	serverReady := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		runProcess(ctx, "server", "bin/server", "logs/server.log", serverReady)
	}()

	select {
	case <-serverReady:
		log.Println("[✓] Server healthy")
	case <-ctx.Done():
		wg.Wait()
		log.Println("[✓] Shutdown complete")
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		runProcess(ctx, "worker", "bin/worker", "logs/worker.log", nil)
	}()

	log.Println("[✓] All services running")
	log.Printf("    Dashboard  → http://localhost:8080")
	log.Printf("    Temporal   → http://localhost:8088")
	log.Printf("    Server log → tail -f logs/server.log")
	log.Printf("    Worker log → tail -f logs/worker.log")
	log.Printf("    Stop       → kill %d  (or: make stop)", os.Getpid())

	<-ctx.Done()
	wg.Wait()
	log.Println("[✓] Shutdown complete")
}

// runProcess runs binary as a child process, restarting on crash up to maxRestarts times.
// If readyCh is non-nil, it is closed once the server health check passes (first start only).
// On context cancellation (graceful shutdown), sends SIGTERM and waits up to 10s before SIGKILL.
func runProcess(ctx context.Context, name, binary, logFile string, readyCh chan<- struct{}) {
	var readyOnce sync.Once

	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt >= maxRestarts {
			log.Printf("[!] %s: %d consecutive crashes — supervisor exiting", name, maxRestarts)
			os.Exit(1)
		}
		if attempt > 0 {
			delay := time.Duration(attempt*attempt) * 5 * time.Second
			log.Printf("[~] %s: restarting in %v (attempt %d/%d)...", name, delay, attempt, maxRestarts)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return
			}
		}

		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("[!] %s: open log: %v", name, err)
			continue
		}

		cmd := exec.Command(binary)
		cmd.Stdout = f
		cmd.Stderr = f

		if err := cmd.Start(); err != nil {
			f.Close()
			log.Printf("[!] %s: failed to start: %v", name, err)
			continue
		}
		log.Printf("[✓] %s started (PID %d)", name, cmd.Process.Pid)

		if readyCh != nil {
			go func() {
				waitHealth(serverHealthURL, 30*time.Second)
				readyOnce.Do(func() { close(readyCh) })
			}()
		}

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			f.Close()
			if ctx.Err() != nil {
				return // clean context cancellation, not a crash
			}
			log.Printf("[!] %s exited unexpectedly: %v", name, err)
			// loop → restart

		case <-ctx.Done():
			log.Printf("[~] %s: sending SIGTERM (PID %d)...", name, cmd.Process.Pid)
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				log.Printf("[!] %s: SIGTERM timeout — force killing", name)
				cmd.Process.Kill()
				<-done
			}
			f.Close()
			log.Printf("[✓] %s stopped", name)
			return
		}
	}
}

// buildBinaries compiles server and worker in parallel. Exits on any error.
func buildBinaries() {
	log.Print("[~] Building server and worker...")
	type result struct {
		name string
		out  []byte
		err  error
	}
	ch := make(chan result, 2)
	for _, t := range []struct{ name, path string }{
		{"server", "./cmd/server"},
		{"worker", "./cmd/worker"},
	} {
		go func(name, path string) {
			out, err := exec.Command("go", "build", "-o", "bin/"+name, path).CombinedOutput()
			ch <- result{name, out, err}
		}(t.name, t.path)
	}
	for i := 0; i < 2; i++ {
		r := <-ch
		if r.err != nil {
			log.Fatalf("[!] build %s failed:\n%s", r.name, r.out)
		}
	}
	log.Println("[✓] Binaries built")
}

// startDocker runs docker compose up -d. Non-fatal — Docker may already be running.
func startDocker() {
	log.Print("[~] Starting Docker services...")
	out, err := exec.Command("docker", "compose", "up", "-d").CombinedOutput()
	if err != nil {
		log.Printf("[~] docker compose up: %v\n%s", err, out)
	}
}

// startCaffeinate prevents macOS from sleeping while the supervisor runs.
// Uses -w <supervisor_pid> so caffeinate exits automatically when supervisor dies.
func startCaffeinate() {
	if runtime.GOOS != "darwin" {
		return
	}
	c := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := c.Start(); err != nil {
		log.Printf("[~] caffeinate unavailable: %v", err)
		return
	}
	log.Printf("[✓] Sleep prevention active")
}

// killOrphans kills stale go-run workers from previous dev sessions.
// They compete on the same Temporal task queue and run old code.
func killOrphans() {
	for _, pat := range []string{"go-build.*worker", "tmp.*exe/worker"} {
		exec.Command("pkill", "-9", "-f", pat).Run()
	}
}

// waitPort blocks until addr is accepting TCP connections. No timeout —
// if Docker is not running this will block until killed.
func waitPort(addr, name string) {
	fmt.Printf("[~] Waiting for %s", name)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			fmt.Println(" — ready")
			return
		}
		fmt.Print(".")
		time.Sleep(time.Second)
	}
}

// waitHealth polls the server's /health endpoint until 200 or timeout.
func waitHealth(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("[~] server health check timed out after %v — worker starting anyway", timeout)
}

func writePID(path string) {
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Fatalf("write supervisor PID: %v", err)
	}
}
