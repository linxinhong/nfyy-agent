package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

type telemetryTestStore struct{}

func (telemetryTestStore) SnapshotSubject(context.Context, memory.SubjectScope) (memory.ControlSnapshot, error) {
	return memory.ControlSnapshot{}, nil
}
func (telemetryTestStore) Snapshot(context.Context, memory.Scope, string) (memory.ControlSnapshot, error) {
	return memory.ControlSnapshot{}, nil
}
func (telemetryTestStore) ReadRecord(context.Context, memory.Scope, string) (*memory.Record, memory.ControlSnapshot, error) {
	return nil, memory.ControlSnapshot{}, nil
}
func (telemetryTestStore) ListRecords(context.Context, memory.ListRequest) (memory.ListResult, error) {
	return memory.ListResult{}, nil
}
func (telemetryTestStore) CommitMutation(context.Context, memory.Mutation) (memory.MutationResult, error) {
	return memory.MutationResult{}, nil
}

func acceptableSafety() correctnessReport {
	return correctnessReport{
		CASWinners: 1, CASConflicts: 31, CASOverloads: 32,
		QuotaRejections: 64,
	}
}

func TestEvaluateThresholdsRecallUsesFrozenSuccessRateContract(t *testing.T) {
	attempts := make([]attempt, 1_000)
	for i := range attempts {
		attempts[i] = attempt{Operation: "recall", LatencyUS: 10_000, Status: "success"}
	}
	// This timeout stays in the attempt denominator and latency sample. At
	// 999/1000 success it meets the frozen >=99.9% Recall contract.
	attempts[len(attempts)-1] = attempt{Operation: "recall", LatencyUS: 1_000_000, Status: "timeout"}
	metric := summarize(attempts)["recall"]
	if metric.Attempts != 1_000 || metric.Successes != 999 || metric.Timeouts != 1 || metric.SuccessRate != .999 || metric.MaxUS != 1_000_000 {
		t.Fatalf("timeout was not retained in the complete metric: %+v", metric)
	}
	out := report{
		Scenarios:     []scenarioReport{{ByOperation: map[string]metrics{"recall": metric}}},
		Correctness:   acceptableSafety(),
		ThresholdPass: make(map[string]bool),
	}
	if err := evaluateThresholds(&out); err != nil {
		t.Fatal(err)
	}
	if !out.ThresholdPass["recall"] {
		t.Fatalf("frozen Recall contract should accept 999/1000 success with compliant percentiles: %+v", metric)
	}
}

func TestEvaluateThresholdsRejectsRecallBelowFrozenSuccessRate(t *testing.T) {
	out := report{
		Scenarios: []scenarioReport{{ByOperation: map[string]metrics{"recall": {
			Attempts: 1_000, Successes: 998, Errors: 2, SuccessRate: .998, P95US: 10_000, P99US: 10_000,
		}}}},
		Correctness:   acceptableSafety(),
		ThresholdPass: make(map[string]bool),
	}
	if err := evaluateThresholds(&out); err != nil {
		t.Fatal(err)
	}
	if out.ThresholdPass["recall"] {
		t.Fatal("Recall below 99.9% success must fail")
	}
}

func TestEvaluateThresholdsSafetyRemainsZeroTolerance(t *testing.T) {
	safety := acceptableSafety()
	safety.CASOtherErrors = 1
	out := report{
		Scenarios: []scenarioReport{{ByOperation: map[string]metrics{"recall": {
			Attempts: 1_000, Successes: 1_000, SuccessRate: 1, P95US: 10_000, P99US: 10_000,
		}}}},
		Correctness:   safety,
		ThresholdPass: make(map[string]bool),
	}
	if err := evaluateThresholds(&out); err != nil {
		t.Fatal(err)
	}
	if out.ThresholdPass["safety"] {
		t.Fatal("unexpected CAS errors must fail safety")
	}
}

func TestAttemptCollectorConcurrentRetainsEveryAttempt(t *testing.T) {
	const workers = 32
	const perWorker = 1_000
	var collector attemptCollector
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				collector.append(attempt{Sequence: int64(worker*perWorker + i), Operation: "recall", Status: "success"})
			}
		}(worker)
	}
	wg.Wait()
	attempts := collector.snapshot()
	if len(attempts) != workers*perWorker {
		t.Fatalf("got %d attempts, want %d", len(attempts), workers*perWorker)
	}
	seen := make(map[int64]struct{}, len(attempts))
	for _, item := range attempts {
		if _, duplicate := seen[item.Sequence]; duplicate {
			t.Fatalf("duplicate sequence %d", item.Sequence)
		}
		seen[item.Sequence] = struct{}{}
	}
}

func TestValidateBuildBindingRequiresEmbeddedAndCurrentValues(t *testing.T) {
	oldCommit, oldRunnerHash, oldProductHash := buildCommit, buildRunnerSourceSHA256, buildProductSHA256
	t.Cleanup(func() {
		buildCommit, buildRunnerSourceSHA256, buildProductSHA256 = oldCommit, oldRunnerHash, oldProductHash
	})
	buildCommit, buildRunnerSourceSHA256, buildProductSHA256 = "commit", "runner", "product"
	cfg := config{BuildCommit: "commit", RunnerSourceHash: "runner", ProductHash: "product"}
	proof := provenance{
		RunnerGitCommit: "commit", ProductGitCommit: "commit",
		RunnerSourceSHA256: "runner", ProductSHA256: "product", GitCleanBefore: true,
	}
	if err := validateBuildBindingFields(cfg, proof); err != nil {
		t.Fatalf("matching embedded/runtime/current values rejected: %v", err)
	}
	cfg.ProductHash = "different"
	if err := validateBuildBindingFields(cfg, proof); err == nil {
		t.Fatal("mismatched runtime product hash accepted")
	}
	cfg.ProductHash = "product"
	proof.GitCleanBefore = false
	proof.GitStatusBefore = " M cmd/memory-gate-load/main.go"
	if err := validateBuildBindingFields(cfg, proof); err == nil {
		t.Fatal("dirty current tree accepted")
	}
}

func TestValidateBuildInfoRequiresExpectedVCSAndModules(t *testing.T) {
	oldCommit := buildCommit
	buildCommit = "fixed"
	t.Cleanup(func() { buildCommit = oldCommit })
	valid := &debug.BuildInfo{
		Path:     "github.com/linxinhong/xingding-agent-runtime/cmd/memory-gate-load",
		Main:     debug.Module{Path: "github.com/linxinhong/xingding-agent-runtime"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "fixed"}, {Key: "vcs.modified", Value: "false"}},
	}
	if err := validateBuildInfo(valid); err != nil {
		t.Fatalf("valid build info rejected: %v", err)
	}
	invalid := *valid
	invalid.Settings = []debug.BuildSetting{{Key: "vcs.revision", Value: "fixed"}, {Key: "vcs.modified", Value: "true"}}
	if err := validateBuildInfo(&invalid); err == nil {
		t.Fatal("modified build accepted")
	}
}

func TestFormalEndpointAndDurabilityValidation(t *testing.T) {
	endpoint, err := parseFormalDSN("postgres://user:secret@127.0.0.1:5432/memory_gate_x")
	if err != nil || endpoint != (dsnEndpoint{Host: "127.0.0.1", Port: "5432", Database: "memory_gate_x", User: "user"}) {
		t.Fatalf("valid loopback endpoint rejected: %#v, %v", endpoint, err)
	}
	if _, err := parseFormalDSN("postgres://user:secret@example.test:5432/memory_gate_x"); err == nil {
		t.Fatal("non-loopback endpoint accepted")
	}
	if _, err := parseFormalDSN("postgres://user:secret@127.0.0.1:5432/memory_gate_x?host=example.test"); err == nil {
		t.Fatal("query authority override accepted")
	}
	if err := validateDockerEndpoint(dockerPortBinding{HostIP: "127.0.0.1", HostPort: "5432"}, endpoint); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerEndpoint(dockerPortBinding{HostIP: "127.0.0.1", HostPort: "5433"}, endpoint); err == nil {
		t.Fatal("mismatched Docker port accepted")
	}
	settings := map[string]string{"server_version": "17.10", "fsync": "on", "synchronous_commit": "on", "full_page_writes": "on"}
	if err := validatePostgreSQLSettings(settings); err != nil {
		t.Fatal(err)
	}
	settings["fsync"] = "off"
	if err := validatePostgreSQLSettings(settings); err == nil {
		t.Fatal("unsafe durability accepted")
	}
}

func TestValidateReferenceProfile(t *testing.T) {
	settings := map[string]string{"server_version": "17.11"}
	for key, value := range referenceProfileSettings {
		settings[key] = value
	}
	profile, err := validateReferenceProfile(config{ReferenceProfile: true}, settings, dockerImageProvenance{NanoCPUs: referenceProfileNanoCPUs, MemoryBytes: referenceProfileMemory, VolumeType: "volume", VolumeName: "isolated-pg-data"})
	if err != nil {
		t.Fatalf("reference profile rejected: %v", err)
	}
	if profile.Name != referenceProfileName || profile.VolumeType != "volume" {
		t.Fatalf("profile evidence = %#v", profile)
	}
	settings["wal_buffers"] = "8MB"
	if _, err := validateReferenceProfile(config{ReferenceProfile: true}, settings, dockerImageProvenance{NanoCPUs: referenceProfileNanoCPUs, MemoryBytes: referenceProfileMemory, VolumeType: "volume", VolumeName: "isolated-pg-data"}); err == nil {
		t.Fatal("profile accepted wrong wal_buffers")
	}
	settings["wal_buffers"] = referenceProfileSettings["wal_buffers"]
	if _, err := validateReferenceProfile(config{ReferenceProfile: true}, settings, dockerImageProvenance{NanoCPUs: referenceProfileNanoCPUs, MemoryBytes: referenceProfileMemory, VolumeType: "tmpfs"}); err == nil {
		t.Fatal("profile accepted tmpfs")
	}
}

func TestPreflightFailureUsesFixedSafeStage(t *testing.T) {
	secret := "postgres://user:private-password@127.0.0.1:5432/memory_gate_x?sslmode=disable"
	if _, err := parseFormalDSN(secret); err == nil {
		t.Fatal("formal DSN query was accepted")
	}
	cfg := config{
		DSN: secret, ExpectedDatabase: "memory_gate_x", Output: "unused.json", Confirm: confirmation,
		BuildCommit: "fixed", RunnerSourceHash: "runner", ProductHash: "product",
		PGImage: "postgres@sha256:fixed", PGImageDigest: "postgres@sha256:fixed", PGImageID: "sha256:fixed", PGContainer: "isolated",
		ReferenceProfile: true,
		Rows:             1_000_000, RPS: 100, MaxInflight: 32, Warmup: 2 * time.Minute, Duration: 10 * time.Minute, Scenario: "all",
	}
	var stderr bytes.Buffer
	if code := runWithStderr(cfg, &stderr); code != 1 {
		t.Fatalf("formal DSN preflight exit code=%d", code)
	}
	got := stderr.String()
	if got != "memory-gate-load: stage=preflight_dsn status=failed\n" {
		t.Fatalf("safe preflight stage=%q", got)
	}
	if strings.Contains(got, "private-password") || strings.Contains(got, "postgres://") || strings.Contains(got, "sslmode") {
		t.Fatalf("preflight error leaked sensitive input: %q", got)
	}
}

func TestRedactCommandCoversDSNSpellingsAndPasswords(t *testing.T) {
	args := []string{"runner", "-dsn", "postgres://user:secret@127.0.0.1:5432/db?password=query", "--dsn=postgres://user:secret@127.0.0.1:5432/db", "-dsn=postgres://user:secret@127.0.0.1:5432/db", "PGPASSWORD=secret", "password=secret"}
	redacted := redactCommand(args)
	for _, value := range redacted {
		if value == "secret" || value == args[2] || value == args[3] || value == args[4] || value == "PGPASSWORD=secret" || value == "password=secret" {
			t.Fatalf("secret argument was retained: %q", value)
		}
	}
	if err := validateAuditFields(config{DSN: "postgres://user:secret@127.0.0.1:5432/db\n"}); err == nil {
		t.Fatal("control character accepted in audit field")
	}
}

func TestSmokeDatabaseParseFailureDoesNotLeakDSNOrQueryPassword(t *testing.T) {
	const secret = "smoke-query-password-must-not-leak"
	cfg := config{
		DSN:              "postgres://smoke:" + secret + "@127.0.0.1:1/memory_gate_smoke?invalid_option=" + secret,
		ExpectedDatabase: "memory_gate_smoke",
		Confirm:          confirmation,
		Output:           filepath.Join(t.TempDir(), "smoke.json"),
		Rows:             1_000,
		RPS:              1,
		MaxInflight:      1,
		Duration:         time.Nanosecond,
		Scenario:         "recall",
		Smoke:            true,
	}
	var stderr bytes.Buffer
	if exitCode := runWithStderr(cfg, &stderr); exitCode != 1 {
		t.Fatalf("got exit code %d, want database failure", exitCode)
	}
	for _, forbidden := range []string{secret, "invalid_option", "postgres://"} {
		if strings.Contains(stderr.String(), forbidden) {
			t.Fatalf("database failure leaked %q in stderr: %q", forbidden, stderr.String())
		}
	}
}

func TestFinalErrorBoundaryRedactsSubsequentDatabaseFailure(t *testing.T) {
	const secret = "subsequent-stage-query-password-must-not-leak"
	driverError := fmt.Errorf("seed records: failed to connect with postgres://user:%s@127.0.0.1/db?password=%s", secret, secret)
	var stderr bytes.Buffer
	writeRunnerFailure(&stderr, safeDatabaseError("database_seed", driverError))
	got := stderr.String()
	if !strings.Contains(got, "stage=database_seed status=failed") {
		t.Fatalf("database stage code missing from stderr: %q", got)
	}
	for _, forbidden := range []string{secret, "postgres://", "password="} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("subsequent database failure leaked %q in stderr: %q", forbidden, got)
		}
	}

	stderr.Reset()
	writeRunnerFailure(&stderr, driverError)
	if got := stderr.String(); got != "memory-gate-load: stage=runner status=failed\n" {
		t.Fatalf("unwrapped failure was not safely classified: %q", got)
	}
}

func TestFlagFailuresDoNotExposeEnvironmentDSN(t *testing.T) {
	const secret = "flag-default-password-must-not-leak"
	envDSN := "postgres://smoke:" + secret + "@127.0.0.1:5432/memory_gate_smoke?password=" + secret
	getenv := func(key string) string {
		if key == "POSTGRES_TEST_DSN" {
			return envDSN
		}
		return ""
	}
	for _, args := range [][]string{{"--help"}, {"--unknown-flag"}, {"--rps=not-an-int"}} {
		var stderr bytes.Buffer
		if exitCode := runMain(args, getenv, &stderr); exitCode != 2 {
			t.Fatalf("args %q got exit code %d, want 2", args, exitCode)
		}
		got := stderr.String()
		if got != "memory-gate-load: stage=arguments status=failed\n" {
			t.Fatalf("args %q produced unsafe or unexpected stderr: %q", args, got)
		}
		for _, forbidden := range []string{secret, envDSN, "postgres://", "password="} {
			if strings.Contains(got, forbidden) {
				t.Fatalf("args %q leaked %q in stderr: %q", args, forbidden, got)
			}
		}
	}
	cfg, err := parseFlags([]string{"--smoke"}, getenv)
	if err != nil || cfg.DSN != envDSN {
		t.Fatal("successful parse did not backfill environment DSN")
	}
}

func TestDiagnosticTelemetryIsNonFormalOnly(t *testing.T) {
	cfg := config{
		DSN: "postgres://user@127.0.0.1:5432/memory_gate_test", ExpectedDatabase: "memory_gate_test", Output: "out.json", Confirm: confirmation,
		Rows: 1_000_000, RPS: 100, MaxInflight: 32, Warmup: 2 * time.Minute, Duration: 10 * time.Minute, Scenario: "all", DiagnosticTelemetry: true,
	}
	if err := run(cfg); err == nil || safeRunnerError(err).Error() != "stage=preflight_config status=failed" {
		t.Fatalf("formal diagnostic telemetry was accepted or classified unsafely: %v", err)
	}
}

func TestAttemptTelemetryTracksOnlyTimingAndDeadlinePhase(t *testing.T) {
	trace := &attemptTrace{}
	ctx := context.WithValue(context.Background(), attemptTraceKey{}, trace)
	store := telemetryStore{AtomicStore: telemetryTestStore{}}
	if _, err := store.Snapshot(ctx, memory.Scope{Namespace: "n", ProjectID: "p", SubjectID: "s", CollectionID: "c"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRecords(ctx, memory.ListRequest{}); err != nil {
		t.Fatal(err)
	}
	runnerRecallObserver{}.ObserveRecall(ctx, memory.RecallTelemetry{Ranking: 7 * time.Microsecond, DeadlineAt: "list_records"})
	if trace.tokenSnapshotUS < 0 || trace.dbListRecordsUS < 0 || trace.rankingUS != 7 || trace.deadlineCause != "list_records" {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestDiagnosticDeltasPreserveCountersAndSignedMemoryChanges(t *testing.T) {
	checkpoint := checkpointDelta(diagnosticCheckpoint{Timed: 5, Requested: 7, WriteMS: 11, SyncMS: 13, BuffersWritten: 17}, diagnosticCheckpoint{Timed: 2, Requested: 3, WriteMS: 5, SyncMS: 8, BuffersWritten: 11})
	if checkpoint != (diagnosticCheckpoint{Timed: 3, Requested: 4, WriteMS: 6, SyncMS: 5, BuffersWritten: 6}) {
		t.Fatalf("checkpoint delta = %+v", checkpoint)
	}
	wal := walDelta(diagnosticWAL{Records: 9, FullPage: 8, Bytes: 7, BuffersFull: 6, Writes: 5, Syncs: 4}, diagnosticWAL{Records: 1, FullPage: 2, Bytes: 3, BuffersFull: 4, Writes: 1, Syncs: 1})
	if wal != (diagnosticWAL{Records: 8, FullPage: 6, Bytes: 4, BuffersFull: 2, Writes: 4, Syncs: 3}) {
		t.Fatalf("wal delta = %+v", wal)
	}
	runtime := runtimeDelta(runtimeCounters{gcPauses: 5, gcPauseMS: 9, heapBytes: 10, goroutines: 3}, runtimeCounters{gcPauses: 2, gcPauseMS: 4, heapBytes: 20, goroutines: 5})
	if runtime != (diagnosticRuntime{GCPauseCount: 3, GCPauseMS: 5, HeapBytes: -10, Goroutines: -2}) {
		t.Fatalf("runtime delta = %+v", runtime)
	}
}

func TestHistogramPauseCountersExcludeInfiniteBucketFromJSONMetric(t *testing.T) {
	count, milliseconds := histogramPauseCounters([]float64{0, .001, math.Inf(1)}, []uint64{2, 3})
	if count != 5 || milliseconds != 2 {
		t.Fatalf("pause counters = count %d milliseconds %v", count, milliseconds)
	}
}

func completeFormalReport() report {
	buildScenario := func(name string, first, last int64) scenarioReport {
		var attempts []attempt
		for sequence := first; sequence <= last; sequence++ {
			scheduled := (sequence - first) * 10_000
			attempts = append(attempts, attempt{Sequence: sequence, Scenario: name, Operation: scheduledOperation(name, sequence), ScheduledOffsetUS: scheduled, StartOffsetUS: scheduled, EndOffsetUS: scheduled + 1_000, LatencyUS: 1_000, Status: "success"})
		}
		return scenarioReport{Name: name, Attempts: attempts, ByOperation: summarize(attempts)}
	}
	recall := buildScenario("recall", 12_000, 71_999)
	mixed := buildScenario("mixed", 1_012_000, 1_071_999)
	return report{Formal: true, Scenarios: []scenarioReport{recall, mixed}, Correctness: acceptableSafety(), ThresholdPass: make(map[string]bool)}
}

func TestEvaluateThresholdsRequiresExactFormalAttemptAccounting(t *testing.T) {
	out := completeFormalReport()
	if err := evaluateThresholds(&out); err != nil {
		t.Fatalf("complete formal attempt log rejected: %v", err)
	}
	if !out.ThresholdPass["recall"] || !out.ThresholdPass["write"] || !out.ThresholdPass["safety"] {
		t.Fatalf("complete formal report did not pass: %+v", out.ThresholdPass)
	}

	for _, mutate := range []struct {
		name  string
		apply func(*report)
	}{
		{"empty", func(out *report) { out.Scenarios = nil }},
		{"missing-operation", func(out *report) { delete(out.Scenarios[1].ByOperation, "revise") }},
		{"inconsistent-count", func(out *report) {
			metric := out.Scenarios[1].ByOperation["create"]
			metric.Attempts--
			out.Scenarios[1].ByOperation["create"] = metric
		}},
		{"duplicate-sequence", func(out *report) {
			out.Scenarios[1].Attempts[0].Sequence = out.Scenarios[0].Attempts[0].Sequence
			out.Scenarios[1].ByOperation = summarize(out.Scenarios[1].Attempts)
		}},
		{"scheduled-offset", func(out *report) {
			out.Scenarios[0].Attempts[1].ScheduledOffsetUS++
			out.Scenarios[0].ByOperation = summarize(out.Scenarios[0].Attempts)
		}},
		{"invalid-status", func(out *report) {
			out.Scenarios[0].Attempts[1].Status = "unknown"
			out.Scenarios[0].ByOperation = summarize(out.Scenarios[0].Attempts)
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			candidate := completeFormalReport()
			mutate.apply(&candidate)
			if err := evaluateThresholds(&candidate); err == nil {
				t.Fatal("invalid formal accounting was accepted")
			}
		})
	}
}

func TestEvaluateThresholdsFormalWritesRejectNonSuccess(t *testing.T) {
	out := completeFormalReport()
	for i := range out.Scenarios[1].Attempts {
		if out.Scenarios[1].Attempts[i].Operation == "create" {
			out.Scenarios[1].Attempts[i].Status = "timeout"
			out.Scenarios[1].Attempts[i].EndOffsetUS = out.Scenarios[1].Attempts[i].ScheduledOffsetUS + 1_000_000
			out.Scenarios[1].Attempts[i].LatencyUS = 1_000_000
			break
		}
	}
	out.Scenarios[1].ByOperation = summarize(out.Scenarios[1].Attempts)
	if err := evaluateThresholds(&out); err != nil {
		t.Fatal(err)
	}
	if out.ThresholdPass["write"] {
		t.Fatal("formal write timeout must fail the write gate")
	}
}
