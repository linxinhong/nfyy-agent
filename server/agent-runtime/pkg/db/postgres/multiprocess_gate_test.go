package postgres

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/linxinhong/xingding-agent-runtime/pkg/execution"
	"github.com/linxinhong/xingding-agent-runtime/pkg/run"
	"github.com/linxinhong/xingding-agent-runtime/pkg/session"
)

const postgresGateRoleEnv = "XINGDING_POSTGRES_GATE_ROLE"

type postgresGateMessage struct {
	Stage    string     `json:"stage"`
	Claim    *run.Claim `json:"claim,omitempty"`
	Revision uint64     `json:"revision,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// TestPostgresManagedStoreMultiProcessTakeoverGate proves fencing across two
// OS processes and two independent PostgreSQL pools. Worker A's stale commit
// is deliberately attempted after B claims but before B's first Run write.
func TestPostgresManagedStoreMultiProcessTakeoverGate(t *testing.T) {
	if os.Getenv(postgresGateRoleEnv) != "" {
		runPostgresGateHelper(t)
		return
	}
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		if os.Getenv("POSTGRES_TEST_FAIL_ON_SKIP") != "" {
			t.Fatal("POSTGRES_TEST_DSN is required (POSTGRES_TEST_FAIL_ON_SKIP=1)")
		}
		t.Skip("POSTGRES_TEST_DSN is not set")
	}

	ctx := context.Background()
	namespace := "multiprocess-gate-" + uuid.NewString()
	runID := "run-" + uuid.NewString()
	sessionID := "session-" + uuid.NewString()
	parent, err := New(ctx, dsn, Options{AutoMigrate: true})
	if err != nil {
		t.Fatalf("open parent PostgreSQL store: %v", err)
	}
	if err := parent.CreateSessionScoped(ctx, &session.AgentSession{
		SessionID: sessionID, AgentID: "gate-agent", UserID: "gate-user", Namespace: namespace,
	}); err != nil {
		_ = parent.Close()
		t.Fatalf("create managed parent: %v", err)
	}
	if err := parent.Close(); err != nil {
		t.Fatalf("close parent PostgreSQL store: %v", err)
	}

	workerA := startPostgresGateChild(t, "a", namespace, runID, sessionID, 300*time.Millisecond, 0)
	claimedA := workerA.read(t, "worker A claim")
	if claimedA.Stage != "claimed" || claimedA.Claim == nil || claimedA.Revision != 1 {
		t.Fatalf("worker A first message = %+v", claimedA)
	}

	workerB := startPostgresGateChild(t, "b", namespace, runID, sessionID, 2*time.Second, claimedA.Revision)
	claimedB := workerB.read(t, "worker B takeover")
	if claimedB.Stage != "claimed" || claimedB.Claim == nil {
		t.Fatalf("worker B first message = %+v", claimedB)
	}
	if claimedB.Claim.FencingToken <= claimedA.Claim.FencingToken {
		t.Fatalf("takeover generation=%d, want > %d", claimedB.Claim.FencingToken, claimedA.Claim.FencingToken)
	}

	workerA.write(t, "stale\n")
	stale := workerA.read(t, "worker A stale commit")
	if stale.Stage != "stale_rejected" {
		t.Fatalf("worker A stale result = %+v", stale)
	}
	workerA.wait(t)

	// B is blocked on the parent barrier, so this read proves A was rejected
	// before the successor's first Run write rather than merely losing a final
	// last-write-wins race.
	inspect, err := New(ctx, dsn, Options{})
	if err != nil {
		t.Fatalf("open inspection store: %v", err)
	}
	snapshot, err := NewLeaseDispatcher(inspect).GetManagedRun(ctx, execution.NamespaceScope{Namespace: namespace}, runID)
	if err != nil {
		_ = inspect.Close()
		t.Fatalf("snapshot before successor write: %v", err)
	}
	if snapshot.Revision != claimedA.Revision || snapshot.Run.Status != session.RunStatusRunning {
		_ = inspect.Close()
		t.Fatalf("stale write changed snapshot: revision=%d status=%s", snapshot.Revision, snapshot.Run.Status)
	}
	if err := inspect.Close(); err != nil {
		t.Fatalf("close inspection store: %v", err)
	}

	workerB.write(t, "commit\n")
	completed := workerB.read(t, "worker B commit")
	if completed.Stage != "completed" || completed.Revision != 2 {
		t.Fatalf("worker B completion = %+v", completed)
	}
	workerB.wait(t)
	t.Logf("multiprocess_takeover,worker_a_pid=%d,worker_b_pid=%d,generation_a=%d,generation_b=%d,revision_before_successor=%d,revision_after_successor=%d,stale_before_successor_write=true",
		workerA.cmd.Process.Pid, workerB.cmd.Process.Pid, claimedA.Claim.FencingToken, claimedB.Claim.FencingToken,
		snapshot.Revision, completed.Revision)
}

// This test is invoked as a subprocess by the gate above. With no role it is
// inert, so ordinary package test discovery does not create database workers.
func TestPostgresManagedStoreProcessHelper(t *testing.T) {
	if os.Getenv(postgresGateRoleEnv) == "" {
		return
	}
	runPostgresGateHelper(t)
}

func runPostgresGateHelper(t *testing.T) {
	role := os.Getenv(postgresGateRoleEnv)
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	namespace := os.Getenv("XINGDING_POSTGRES_GATE_NAMESPACE")
	runID := os.Getenv("XINGDING_POSTGRES_GATE_RUN_ID")
	sessionID := os.Getenv("XINGDING_POSTGRES_GATE_SESSION_ID")
	leaseNanos, err := strconv.ParseInt(os.Getenv("XINGDING_POSTGRES_GATE_LEASE_NS"), 10, 64)
	if err != nil {
		emitPostgresGateFailure(t, "parse lease: %v", err)
	}
	store, err := New(context.Background(), dsn, Options{})
	if err != nil {
		emitPostgresGateFailure(t, "open PostgreSQL: %v", err)
	}
	defer store.Close()
	dispatcher := NewLeaseDispatcher(store)
	scope := execution.NamespaceScope{Namespace: namespace}
	lease := time.Duration(leaseNanos)
	workerID := "gate-worker-" + role
	var claim *run.Claim
	if role == "a" {
		claim, err = dispatcher.Claim(context.Background(), scope, runID, run.WorkerIdentity{WorkerID: workerID}, lease)
	} else {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			claim, err = dispatcher.Claim(context.Background(), scope, runID, run.WorkerIdentity{WorkerID: workerID}, lease)
			if err == nil {
				break
			}
			if !errors.Is(err, run.ErrLeaseHeld) {
				break
			}
			timer := time.NewTimer(5 * time.Millisecond)
			<-timer.C
		}
	}
	if err != nil || claim == nil {
		emitPostgresGateFailure(t, "claim role %s: %v", role, err)
	}

	revision := uint64(0)
	if role == "a" {
		output := &session.RunOutput{
			RunID: runID, SessionID: sessionID, AgentID: "gate-agent", UserID: "gate-user",
			Namespace: namespace, Status: session.RunStatusRunning,
		}
		revision, err = dispatcher.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{Run: output})
		if err != nil {
			emitPostgresGateFailure(t, "initial commit: %v", err)
		}
	} else {
		revision, err = strconv.ParseUint(os.Getenv("XINGDING_POSTGRES_GATE_EXPECTED_REVISION"), 10, 64)
		if err != nil {
			emitPostgresGateFailure(t, "parse expected revision: %v", err)
		}
	}
	emitPostgresGateMessage(postgresGateMessage{Stage: "claimed", Claim: claim, Revision: revision})

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		emitPostgresGateFailure(t, "wait for parent barrier: %v", scanner.Err())
	}
	output := &session.RunOutput{
		RunID: runID, SessionID: sessionID, AgentID: "gate-agent", UserID: "gate-user",
		Namespace: namespace, Status: session.RunStatusCompleted,
	}
	committed, commitErr := dispatcher.CommitManagedRun(context.Background(), scope, claim, run.ManagedRunCommit{
		Run: output, ExpectedRevision: revision, ExpectedStatus: session.RunStatusRunning,
	})
	if role == "a" {
		if !errors.Is(commitErr, run.ErrStaleFencingToken) {
			emitPostgresGateFailure(t, "stale owner commit: %v", commitErr)
		}
		emitPostgresGateMessage(postgresGateMessage{Stage: "stale_rejected"})
		return
	}
	if commitErr != nil {
		emitPostgresGateFailure(t, "successor commit: %v", commitErr)
	}
	emitPostgresGateMessage(postgresGateMessage{Stage: "completed", Revision: committed})
}

type postgresGateChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	decode *json.Decoder
	stderr bytes.Buffer
}

func startPostgresGateChild(t *testing.T, role, namespace, runID, sessionID string, lease time.Duration, revision uint64) *postgresGateChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPostgresManagedStoreProcessHelper$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(),
		postgresGateRoleEnv+"="+role,
		"XINGDING_POSTGRES_GATE_NAMESPACE="+namespace,
		"XINGDING_POSTGRES_GATE_RUN_ID="+runID,
		"XINGDING_POSTGRES_GATE_SESSION_ID="+sessionID,
		fmt.Sprintf("XINGDING_POSTGRES_GATE_LEASE_NS=%d", lease.Nanoseconds()),
		fmt.Sprintf("XINGDING_POSTGRES_GATE_EXPECTED_REVISION=%d", revision),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("%s stdin: %v", role, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("%s stdout: %v", role, err)
	}
	child := &postgresGateChild{cmd: cmd, stdin: stdin, decode: json.NewDecoder(stdout)}
	cmd.Stderr = &child.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker %s: %v", role, err)
	}
	t.Cleanup(func() {
		if child.cmd.ProcessState == nil {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
	})
	return child
}

func (c *postgresGateChild) read(t *testing.T, stage string) postgresGateMessage {
	t.Helper()
	type result struct {
		message postgresGateMessage
		err     error
	}
	ready := make(chan result, 1)
	go func() {
		var message postgresGateMessage
		err := c.decode.Decode(&message)
		ready <- result{message: message, err: err}
	}()
	select {
	case result := <-ready:
		if result.err != nil {
			t.Fatalf("%s decode: %v; stderr=%s", stage, result.err, c.stderr.String())
		}
		if result.message.Error != "" {
			t.Fatalf("%s child error: %s; stderr=%s", stage, result.message.Error, c.stderr.String())
		}
		return result.message
	case <-time.After(8 * time.Second):
		t.Fatalf("%s timed out; stderr=%s", stage, c.stderr.String())
		return postgresGateMessage{}
	}
}

func (c *postgresGateChild) write(t *testing.T, value string) {
	t.Helper()
	if _, err := io.WriteString(c.stdin, value); err != nil {
		t.Fatalf("signal child: %v; stderr=%s", err, c.stderr.String())
	}
}

func (c *postgresGateChild) wait(t *testing.T) {
	t.Helper()
	if err := c.stdin.Close(); err != nil {
		t.Fatalf("close child stdin: %v", err)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v; stderr=%s", err, c.stderr.String())
	}
}

func emitPostgresGateMessage(message postgresGateMessage) {
	_ = json.NewEncoder(os.Stdout).Encode(message)
}

func emitPostgresGateFailure(t *testing.T, format string, args ...any) {
	t.Helper()
	message := fmt.Sprintf(format, args...)
	emitPostgresGateMessage(postgresGateMessage{Error: message})
	t.Fatal(message)
}
