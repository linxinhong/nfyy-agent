// Command curator is the curation worker: it leases curation jobs from
// PostgreSQL and turns contributions into proposals (packages/curation).
//
// Why a separate process: curation is slow and memory-hungry (model calls)
// and must not share fate or latency budget with the gateway. It shares the
// database and code, not the process.
//
// Pitfalls: the gateway owns migrations; start the curator against a
// migrated database. Several curators may run at once — jobs are leased with
// SKIP LOCKED and an expired lease returns the job to the queue, so the lease
// must outlast one agent run (lease 5 min > run timeout 3 min).
//
// The model curator runs when EAP_CURATOR_MODEL and EAP_CURATOR_API_KEY are
// set; it calls the gateway's /v1 endpoint so model use passes the same
// Policy/DLP, usage and audit as employees. Without them the baseline
// (model-free) curator runs.
//
// Every curator also schedules the low-frequency global sweep: hourly it
// queues a sweep for each Workspace not swept within EAP_CURATOR_SWEEP_EVERY
// (Go duration, default 24h; 0 disables). The database admits one open
// sweep per Workspace, so several curators never double it.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/curatoragent"
	"enterprise-agent-platform/packages/store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dsn := os.Getenv("EAP_DATABASE_URL")
	if dsn == "" {
		log.Fatal("curator: EAP_DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("curator: database: %v", err)
	}
	defer pool.Close()
	host, _ := os.Hostname()
	st := store.New(pool)
	contentStore := content.New(pool, st)
	var curator curation.Curator = curation.Baseline{}
	kind := "baseline curator"
	if model := os.Getenv("EAP_CURATOR_MODEL"); model != "" || os.Getenv("EAP_CURATOR_API_KEY") != "" {
		agentCurator, err := curatoragent.New(curatoragent.Config{
			BaseURL:    envOr("EAP_CURATOR_LLM_BASE_URL", "http://127.0.0.1:8080/v1"),
			APIKey:     os.Getenv("EAP_CURATOR_API_KEY"),
			Model:      model,
			RunTimeout: 3 * time.Minute,
		})
		if err != nil {
			log.Fatalf("curator: %v (set both EAP_CURATOR_MODEL and EAP_CURATOR_API_KEY, or neither)", err)
		}
		curator, kind = agentCurator, "model curator "+model
	}
	worker := &curation.Worker{
		Store:   curation.New(pool, st, contentStore),
		Content: contentStore,
		Curator: curator,
		Owner:   envOr("EAP_CURATOR_ID", fmt.Sprintf("%s-%d", host, os.Getpid())),
		Lease:   5 * time.Minute,
		Idle:    5 * time.Second,
	}
	sweepEvery, err := time.ParseDuration(envOr("EAP_CURATOR_SWEEP_EVERY", "24h"))
	if err != nil || sweepEvery < 0 {
		log.Fatalf("curator: EAP_CURATOR_SWEEP_EVERY must be a Go duration such as 24h or 0: %q", os.Getenv("EAP_CURATOR_SWEEP_EVERY"))
	}
	if sweepEvery > 0 {
		go scheduleSweeps(ctx, worker.Store, sweepEvery)
	}
	log.Printf("curator: %s started (%s, sweep every %s)", worker.Owner, kind, sweepEvery)
	worker.Run(ctx)
	log.Printf("curator: stopped")
}

func scheduleSweeps(ctx context.Context, st *curation.Store, every time.Duration) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		if n, err := st.EnqueueSweeps(ctx, every); err != nil {
			log.Printf("curator: schedule sweeps: %v", err)
		} else if n > 0 {
			log.Printf("curator: queued %d sweep(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
