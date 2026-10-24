// Command memory-gate-load runs the reproducible MEM-008 PostgreSQL load.
// It deliberately refuses non-disposable database names and requires an
// explicit destructive confirmation before truncating its isolated database.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	runtimeMetrics "runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pgstore "github.com/linxinhong/xingding-agent-runtime/pkg/db/postgres"
	"github.com/linxinhong/xingding-agent-runtime/pkg/memory"
)

const (
	gateSeed       = int64(20260908)
	confirmation   = "DELETE-ISOLATED-MEMORY-GATE"
	datasetVersion = "mem008-v1-1000-records-per-subject-body-25-50-25-expired-5pct"
)

const referenceProfileName = "mem010-pg17-durable-v1"

var referenceProfileSettings = map[string]string{
	"shared_buffers":               "1GB",
	"checkpoint_timeout":           "5min",
	"checkpoint_completion_target": "0.9",
	"max_wal_size":                 "2GB",
	"min_wal_size":                 "512MB",
	"wal_buffers":                  "16MB",
	"autovacuum":                   "on",
	"fsync":                        "on",
	"synchronous_commit":           "on",
	"full_page_writes":             "on",
}

const (
	referenceProfileNanoCPUs = int64(4_000_000_000)
	referenceProfileMemory   = int64(4 << 30)
)

// These values are set only by the reproducible go build command used for a
// formal run. A plain go run or an unbound binary deliberately cannot run the
// formal protocol.
var (
	buildCommit             string
	buildRunnerSourceSHA256 string
	buildProductSHA256      string
)

type config struct {
	DSN                 string        `json:"-"`
	RedactedDSN         string        `json:"redacted_dsn"`
	ExpectedDatabase    string        `json:"expected_database"`
	Confirm             string        `json:"-"`
	Output              string        `json:"output"`
	BuildCommit         string        `json:"build_commit"`
	RunnerSourceHash    string        `json:"runner_source_sha256"`
	ProductHash         string        `json:"product_sha256"`
	PGImage             string        `json:"pg_image"`
	PGImageDigest       string        `json:"pg_image_digest"`
	PGImageID           string        `json:"pg_image_id"`
	PGContainer         string        `json:"pg_container"`
	ReferenceProfile    bool          `json:"reference_profile"`
	Rows                int           `json:"rows"`
	RPS                 int           `json:"rps"`
	MaxInflight         int           `json:"max_inflight"`
	Warmup              time.Duration `json:"warmup"`
	Duration            time.Duration `json:"duration"`
	Scenario            string        `json:"scenario"`
	Smoke               bool          `json:"smoke"`
	DiagnosticTelemetry bool          `json:"diagnostic_telemetry"`
}

type attempt struct {
	Sequence          int64  `json:"sequence"`
	Scenario          string `json:"scenario"`
	Operation         string `json:"operation"`
	ScheduledOffsetUS int64  `json:"scheduled_offset_us"`
	StartOffsetUS     int64  `json:"start_offset_us"`
	EndOffsetUS       int64  `json:"end_offset_us"`
	LatencyUS         int64  `json:"latency_us"`
	Status            string `json:"status"`
	QueueDelayUS      int64  `json:"queue_delay_us,omitempty"`
	TokenSnapshotUS   int64  `json:"token_snapshot_us,omitempty"`
	DBListRecordsUS   int64  `json:"db_list_records_us,omitempty"`
	RankingUS         int64  `json:"ranking_us,omitempty"`
	OperationUS       int64  `json:"operation_us,omitempty"`
	DeadlineCause     string `json:"internal_deadline_cause,omitempty"`
}

type attemptTraceKey struct{}

// attemptTrace contains timing-only state scoped to one runner request. It
// intentionally never stores a token, query, scope, SQL text, or memory data.
type attemptTrace struct {
	tokenSnapshotUS int64
	dbListRecordsUS int64
	rankingUS       int64
	deadlineCause   string
}

func traceFromContext(ctx context.Context) *attemptTrace {
	trace, _ := ctx.Value(attemptTraceKey{}).(*attemptTrace)
	return trace
}

type telemetryStore struct{ memory.AtomicStore }

func (s telemetryStore) Snapshot(ctx context.Context, scope memory.Scope, sourceRef string) (memory.ControlSnapshot, error) {
	started := time.Now()
	snapshot, err := s.AtomicStore.Snapshot(ctx, scope, sourceRef)
	if trace := traceFromContext(ctx); trace != nil {
		trace.tokenSnapshotUS = time.Since(started).Microseconds()
	}
	return snapshot, err
}

func (s telemetryStore) ListRecords(ctx context.Context, request memory.ListRequest) (memory.ListResult, error) {
	started := time.Now()
	result, err := s.AtomicStore.ListRecords(ctx, request)
	if trace := traceFromContext(ctx); trace != nil {
		trace.dbListRecordsUS = time.Since(started).Microseconds()
	}
	return result, err
}

type runnerRecallObserver struct{}

func (runnerRecallObserver) ObserveRecall(ctx context.Context, trace memory.RecallTelemetry) {
	if target := traceFromContext(ctx); target != nil {
		// ListRecords is measured at the adapter boundary above. The module's
		// ranking timing and deadline phase complete the non-sensitive trace.
		target.rankingUS = trace.Ranking.Microseconds()
		target.deadlineCause = trace.DeadlineAt
	}
}

type attemptCollector struct {
	mu       sync.Mutex
	attempts []attempt
}

func (c *attemptCollector) append(item attempt) {
	c.mu.Lock()
	c.attempts = append(c.attempts, item)
	c.mu.Unlock()
}

func (c *attemptCollector) snapshot() []attempt {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]attempt(nil), c.attempts...)
}

type metrics struct {
	Attempts    int     `json:"attempts"`
	Successes   int     `json:"successes"`
	Errors      int     `json:"errors"`
	Overloads   int     `json:"overloads"`
	Timeouts    int     `json:"timeouts"`
	SuccessRate float64 `json:"success_rate"`
	P50US       int64   `json:"p50_us"`
	P95US       int64   `json:"p95_us"`
	P99US       int64   `json:"p99_us"`
	MaxUS       int64   `json:"max_us"`
}

type scenarioReport struct {
	Name        string                      `json:"name"`
	StartedAt   time.Time                   `json:"started_at"`
	FinishedAt  time.Time                   `json:"finished_at"`
	ByOperation map[string]metrics          `json:"by_operation"`
	Attempts    []attempt                   `json:"attempt_log"`
	Checkpoint  *scenarioCheckpointEvidence `json:"checkpoint,omitempty"`
}

type correctnessReport struct {
	CASWinners       int `json:"cas_winners"`
	CASConflicts     int `json:"cas_conflicts"`
	CASOverloads     int `json:"cas_overloads"`
	CASOtherErrors   int `json:"cas_other_errors"`
	QuotaSuccesses   int `json:"quota_successes"`
	QuotaRejections  int `json:"quota_rejections"`
	QuotaOtherErrors int `json:"quota_other_errors"`
}

type datasetCounts struct {
	Records  int64 `json:"records"`
	Tenants  int64 `json:"tenants"`
	Subjects int64 `json:"subjects"`
}

type provenance struct {
	WorkingDirectory           string   `json:"working_directory"`
	RepositoryRoot             string   `json:"repository_root"`
	RunnerGitCommit            string   `json:"runner_git_commit"`
	ProductGitCommit           string   `json:"product_git_commit"`
	RunnerSourceSHA256         string   `json:"runner_source_sha256"`
	ProductSHA256              string   `json:"product_sha256"`
	Executable                 string   `json:"executable"`
	ExecutableSHA256           string   `json:"executable_sha256"`
	BuildInfo                  string   `json:"build_info"`
	EmbeddedBuildCommit        string   `json:"embedded_build_commit"`
	EmbeddedRunnerSourceSHA256 string   `json:"embedded_runner_source_sha256"`
	EmbeddedProductSHA256      string   `json:"embedded_product_sha256"`
	GitCleanBefore             bool     `json:"git_clean_before"`
	GitStatusBefore            string   `json:"git_status_before"`
	Command                    []string `json:"redacted_command"`
}

type dockerImageProvenance struct {
	Container         string   `json:"container"`
	ContainerImage    string   `json:"container_image"`
	ImageID           string   `json:"image_id"`
	RepoDigests       []string `json:"repo_digests"`
	PublishedHostIP   string   `json:"published_host_ip"`
	PublishedHostPort string   `json:"published_host_port"`
	NanoCPUs          int64    `json:"nano_cpus"`
	MemoryBytes       int64    `json:"memory_bytes"`
	VolumeType        string   `json:"volume_type"`
	VolumeName        string   `json:"volume_name"`
}

type connectionProvenance struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}

type dsnEndpoint struct {
	Host     string
	Port     string
	Database string
	User     string
}

// runnerFailure deliberately contains only a fixed stage code. PostgreSQL
// drivers may retain a DSN (including query values) in errors returned while
// reconnecting long after the first successful connection.
type runnerFailure struct {
	stage string
}

func (e runnerFailure) Error() string {
	return "stage=" + e.stage + " status=failed"
}

// preflightFailure preserves a precise, fixed diagnostic stage while ensuring
// the final runner boundary cannot render a DSN, Docker output, or filesystem
// detail retained by the underlying error.
func preflightFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return runnerFailure{stage: stage}
}

func readCounts(ctx context.Context, db *sql.DB) (datasetCounts, error) {
	var counts datasetCounts
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM agent_runtime_memory_records),
		(SELECT count(DISTINCT namespace) FROM agent_runtime_memory_subjects),
		(SELECT count(*) FROM agent_runtime_memory_subjects)`).
		Scan(&counts.Records, &counts.Tenants, &counts.Subjects)
	return counts, err
}

func collectProvenance() (provenance, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return provenance{}, fmt.Errorf("get working directory: %w", err)
	}
	git := func(args ...string) (string, error) {
		output, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return provenance{}, err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return provenance{}, err
	}
	status, err := git("status", "--porcelain=v1")
	if err != nil {
		return provenance{}, err
	}
	runnerPath := filepath.Join(root, "cmd", "memory-gate-load", "main.go")
	productPath := filepath.Join(root, "pkg", "db", "postgres", "trusted_memory_store.go")
	runnerHash, err := fileSHA256(runnerPath)
	if err != nil {
		return provenance{}, err
	}
	productHash, err := fileSHA256(productPath)
	if err != nil {
		return provenance{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return provenance{}, fmt.Errorf("locate runner executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return provenance{}, fmt.Errorf("resolve runner executable: %w", err)
	}
	executableHash, err := fileSHA256(executable)
	if err != nil {
		return provenance{}, err
	}
	buildInfo, err := exec.Command("go", "version", "-m", executable).CombinedOutput()
	if err != nil {
		return provenance{}, fmt.Errorf("read runner build info: %w: %s", err, strings.TrimSpace(string(buildInfo)))
	}
	runtimeInfo, err := readRuntimeBuildInfo()
	if err != nil {
		return provenance{}, err
	}
	return provenance{
		WorkingDirectory: cwd, RepositoryRoot: root,
		RunnerGitCommit: head, ProductGitCommit: head,
		RunnerSourceSHA256: runnerHash, ProductSHA256: productHash,
		Executable: executable, ExecutableSHA256: executableHash,
		BuildInfo:                  string(buildInfo) + runtimeInfo,
		EmbeddedBuildCommit:        buildCommit,
		EmbeddedRunnerSourceSHA256: buildRunnerSourceSHA256,
		EmbeddedProductSHA256:      buildProductSHA256,
		GitCleanBefore:             status == "", GitStatusBefore: status,
	}, nil
}

func validateBuildBinding(cfg config, proof provenance) error {
	if err := validateBuildBindingFields(cfg, proof); err != nil {
		return err
	}
	if _, err := validateRuntimeBuildInfo(); err != nil {
		return err
	}
	return nil
}

func validateBuildBindingFields(cfg config, proof provenance) error {
	if buildCommit == "" || buildRunnerSourceSHA256 == "" || buildProductSHA256 == "" {
		return errors.New("formal runner is missing build-time provenance; rebuild with required -ldflags")
	}
	if !proof.GitCleanBefore {
		return fmt.Errorf("formal runner requires a clean worktree before execution: %s", proof.GitStatusBefore)
	}
	if cfg.BuildCommit != buildCommit || cfg.RunnerSourceHash != buildRunnerSourceSHA256 || cfg.ProductHash != buildProductSHA256 {
		return errors.New("runtime provenance parameters do not match values embedded in runner binary")
	}
	if proof.RunnerGitCommit != buildCommit || proof.ProductGitCommit != buildCommit {
		return fmt.Errorf("current tree commit does not match embedded build commit %s", buildCommit)
	}
	if proof.RunnerSourceSHA256 != buildRunnerSourceSHA256 || proof.ProductSHA256 != buildProductSHA256 {
		return errors.New("current runner or product source hash does not match embedded binary provenance")
	}
	return nil
}

func readRuntimeBuildInfo() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "embedded_runtime_build_info=<unavailable>\n", nil
	}
	encoded, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	return "\nembedded_runtime_build_info=" + string(encoded) + "\n", nil
}

func validateRuntimeBuildInfo() (*debug.BuildInfo, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, errors.New("formal runner is missing Go build info")
	}
	if err := validateBuildInfo(info); err != nil {
		return nil, err
	}
	return info, nil
}

func validateBuildInfo(info *debug.BuildInfo) error {
	if info == nil {
		return errors.New("formal runner is missing Go build info")
	}
	const modulePath = "github.com/linxinhong/xingding-agent-runtime"
	if info.Path != modulePath+"/cmd/memory-gate-load" || info.Main.Path != modulePath || info.Main.Replace != nil {
		return errors.New("formal runner was not built as the expected main module")
	}
	for _, dep := range info.Deps {
		if dep.Replace != nil {
			return fmt.Errorf("formal runner has replaced module dependency %q", dep.Path)
		}
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["vcs.revision"] == "" || settings["vcs.modified"] == "" {
		return errors.New("formal runner lacks VCS build settings; rebuild with -buildvcs=true")
	}
	if settings["vcs.revision"] != buildCommit || settings["vcs.modified"] != "false" {
		return errors.New("formal runner VCS build settings do not match immutable embedded provenance")
	}
	return nil
}

func parseFormalDSN(raw string) (dsnEndpoint, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return dsnEndpoint{}, errors.New("formal runner requires a PostgreSQL URL DSN")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || port == "" {
		return dsnEndpoint{}, errors.New("formal runner requires a loopback TCP PostgreSQL DSN with explicit port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return dsnEndpoint{}, errors.New("formal runner DSN port is invalid")
	}
	database := strings.TrimPrefix(parsed.EscapedPath(), "/")
	if database == "" || strings.Contains(database, "/") {
		return dsnEndpoint{}, errors.New("formal runner requires exactly one database name in DSN path")
	}
	user := ""
	if parsed.User != nil {
		user = parsed.User.Username()
	}
	return dsnEndpoint{Host: ip.String(), Port: port, Database: database, User: user}, nil
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerMount struct {
	Type   string `json:"Type"`
	Name   string `json:"Name"`
	Target string `json:"Destination"`
}

func inspectDockerImage(cfg config, endpoint dsnEndpoint) (dockerImageProvenance, error) {
	output, err := exec.Command("docker", "inspect", "--format", "{{.Config.Image}}\n{{.Image}}\n{{json (index .NetworkSettings.Ports \"5432/tcp\")}}\n{{.HostConfig.NanoCpus}}\n{{.HostConfig.Memory}}\n{{json .Mounts}}", cfg.PGContainer).CombinedOutput()
	if err != nil {
		return dockerImageProvenance{}, fmt.Errorf("inspect PostgreSQL container %q: %w: %s", cfg.PGContainer, err, strings.TrimSpace(string(output)))
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(parts) != 6 || parts[0] == "" || parts[1] == "" {
		return dockerImageProvenance{}, fmt.Errorf("unexpected Docker inspect image output for %q", cfg.PGContainer)
	}
	if parts[0] != cfg.PGImage || parts[1] != cfg.PGImageID {
		return dockerImageProvenance{}, errors.New("Docker-inspected image reference or ID does not match formal runtime parameters")
	}
	digestOutput, err := exec.Command("docker", "image", "inspect", "--format", "{{json .RepoDigests}}", parts[1]).CombinedOutput()
	if err != nil {
		return dockerImageProvenance{}, fmt.Errorf("inspect PostgreSQL image %q: %w: %s", parts[1], err, strings.TrimSpace(string(digestOutput)))
	}
	var digests []string
	if err := json.Unmarshal(digestOutput, &digests); err != nil {
		return dockerImageProvenance{}, fmt.Errorf("decode PostgreSQL repo digests: %w", err)
	}
	found := false
	for _, digest := range digests {
		if digest == cfg.PGImageDigest {
			found = true
			break
		}
	}
	if !found {
		return dockerImageProvenance{}, fmt.Errorf("Docker-inspected repo digests do not contain requested digest %q", cfg.PGImageDigest)
	}
	var bindings []dockerPortBinding
	if err := json.Unmarshal([]byte(parts[2]), &bindings); err != nil || len(bindings) != 1 {
		return dockerImageProvenance{}, errors.New("formal PostgreSQL container must expose exactly one published 5432/tcp binding")
	}
	if err := validateDockerEndpoint(bindings[0], endpoint); err != nil {
		return dockerImageProvenance{}, err
	}
	nanoCPUs, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return dockerImageProvenance{}, errors.New("Docker-inspected CPU limit is invalid")
	}
	memoryBytes, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return dockerImageProvenance{}, errors.New("Docker-inspected memory limit is invalid")
	}
	var mounts []dockerMount
	if err := json.Unmarshal([]byte(parts[5]), &mounts); err != nil {
		return dockerImageProvenance{}, errors.New("Docker-inspected mounts are invalid")
	}
	var volume dockerMount
	for _, mount := range mounts {
		if mount.Target == "/var/lib/postgresql/data" {
			volume = mount
			break
		}
	}
	if volume.Type == "" {
		return dockerImageProvenance{}, errors.New("Docker-inspected PostgreSQL data volume is missing")
	}
	bindingIP := net.ParseIP(bindings[0].HostIP)
	return dockerImageProvenance{Container: cfg.PGContainer, ContainerImage: parts[0], ImageID: parts[1], RepoDigests: digests, PublishedHostIP: bindingIP.String(), PublishedHostPort: bindings[0].HostPort, NanoCPUs: nanoCPUs, MemoryBytes: memoryBytes, VolumeType: volume.Type, VolumeName: volume.Name}, nil
}

func validateDockerEndpoint(binding dockerPortBinding, endpoint dsnEndpoint) error {
	bindingIP := net.ParseIP(binding.HostIP)
	if bindingIP == nil || !bindingIP.IsLoopback() || bindingIP.String() != endpoint.Host || binding.HostPort != endpoint.Port {
		return errors.New("formal DSN endpoint does not exactly match Docker's loopback 5432/tcp binding")
	}
	return nil
}

func validateReferenceProfile(cfg config, settings map[string]string, docker dockerImageProvenance) (referenceProfileEvidence, error) {
	if !cfg.ReferenceProfile {
		return referenceProfileEvidence{}, errors.New("reference profile was not requested")
	}
	major := strings.Split(settings["server_version"], ".")[0]
	if major != "17" {
		return referenceProfileEvidence{}, fmt.Errorf("reference profile requires PostgreSQL 17, got %q", settings["server_version"])
	}
	if docker.NanoCPUs != referenceProfileNanoCPUs || docker.MemoryBytes != referenceProfileMemory {
		return referenceProfileEvidence{}, errors.New("Docker CPU or memory limit does not match reference profile")
	}
	if docker.VolumeType != "volume" || docker.VolumeName == "" {
		return referenceProfileEvidence{}, errors.New("reference profile requires a named non-tmpfs PostgreSQL data volume")
	}
	for key, want := range referenceProfileSettings {
		if settings[key] != want {
			return referenceProfileEvidence{}, fmt.Errorf("reference profile requires %s=%s, got %q", key, want, settings[key])
		}
	}
	return referenceProfileEvidence{Name: referenceProfileName, NanoCPUs: docker.NanoCPUs, MemoryBytes: docker.MemoryBytes, VolumeType: docker.VolumeType, VolumeName: docker.VolumeName, Settings: settings}, nil
}

// validatePostgreSQLSettings preserves the basic durable-PG assertion used by
// non-profile unit tests. Formal runs additionally call validateReferenceProfile.
func validatePostgreSQLSettings(settings map[string]string) error {
	major := strings.Split(settings["server_version"], ".")[0]
	if major != "17" {
		return fmt.Errorf("formal runner requires PostgreSQL 17, got %q", settings["server_version"])
	}
	for _, key := range []string{"fsync", "synchronous_commit", "full_page_writes"} {
		if settings[key] != "on" {
			return fmt.Errorf("formal runner requires %s=on, got %q", key, settings[key])
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func redactDSN(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<redacted>"
	}
	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func redactCommand(args []string) []string {
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		if (arg == "-dsn" || arg == "--dsn") && i+1 < len(redacted) {
			redacted[i+1] = redactDSN(redacted[i+1])
		}
		if strings.HasPrefix(arg, "-dsn=") || strings.HasPrefix(arg, "--dsn=") {
			prefix := "-dsn="
			if strings.HasPrefix(arg, "--dsn=") {
				prefix = "--dsn="
			}
			redacted[i] = prefix + redactDSN(strings.TrimPrefix(arg, prefix))
		}
		upper := strings.ToUpper(arg)
		if strings.HasPrefix(upper, "PGPASSWORD=") || strings.Contains(strings.ToLower(arg), "password=") {
			redacted[i] = "<redacted-sensitive-argument>"
		}
	}
	return redacted
}

func validateAuditFields(cfg config) error {
	for _, value := range []string{cfg.DSN, cfg.ExpectedDatabase, cfg.Output, cfg.BuildCommit, cfg.RunnerSourceHash, cfg.ProductHash, cfg.PGImage, cfg.PGImageDigest, cfg.PGImageID, cfg.PGContainer} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("formal runner rejects control characters in recorded parameters")
		}
	}
	return nil
}

func writeInvocation(output string, proof provenance) error {
	encoded, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return fmt.Errorf("encode invocation evidence: %w", err)
	}
	path := output + ".invocation.json"
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write invocation evidence %s: %w", path, err)
	}
	return nil
}

type report struct {
	Protocol         string                    `json:"protocol"`
	Formal           bool                      `json:"formal"`
	Seed             int64                     `json:"seed"`
	DatasetSHA256    string                    `json:"dataset_sha256"`
	Config           config                    `json:"config"`
	Provenance       provenance                `json:"provenance"`
	Environment      map[string]any            `json:"environment"`
	PostgreSQL       map[string]string         `json:"postgresql"`
	DockerImage      dockerImageProvenance     `json:"docker_image"`
	Connection       connectionProvenance      `json:"connection"`
	ReferenceProfile *referenceProfileEvidence `json:"reference_profile,omitempty"`
	PreSeedCounts    datasetCounts             `json:"pre_seed_counts"`
	PostSeedCounts   datasetCounts             `json:"post_seed_counts"`
	PostRunCounts    datasetCounts             `json:"post_run_counts"`
	QueryPlan        json.RawMessage           `json:"query_plan"`
	SeedStartedAt    time.Time                 `json:"seed_started_at"`
	SeedFinishedAt   time.Time                 `json:"seed_finished_at"`
	Scenarios        []scenarioReport          `json:"scenarios"`
	Correctness      correctnessReport         `json:"correctness"`
	Thresholds       map[string]string         `json:"thresholds"`
	ThresholdPass    map[string]bool           `json:"threshold_pass"`
	Diagnostics      *diagnosticReport         `json:"diagnostics,omitempty"`
}

type referenceProfileEvidence struct {
	Name        string            `json:"name"`
	NanoCPUs    int64             `json:"nano_cpus"`
	MemoryBytes int64             `json:"memory_bytes"`
	VolumeType  string            `json:"volume_type"`
	VolumeName  string            `json:"volume_name"`
	Settings    map[string]string `json:"settings"`
}

type checkpointCounters struct {
	Timed     int64 `json:"timed"`
	Requested int64 `json:"requested"`
}

type scenarioCheckpointEvidence struct {
	Start     checkpointCounters `json:"start"`
	End       checkpointCounters `json:"end"`
	Completed int64              `json:"completed"`
}

const diagnosticSampleLimit = 2_000

type diagnosticReport struct {
	IntervalMS     int                `json:"interval_ms"`
	SampleLimit    int                `json:"sample_limit"`
	DroppedSamples int                `json:"dropped_samples"`
	Samples        []diagnosticSample `json:"samples"`
}

type diagnosticSample struct {
	OffsetMS   int64                `json:"offset_ms"`
	Pool       diagnosticPool       `json:"pool"`
	Waits      []diagnosticWait     `json:"pg_waits"`
	Checkpoint diagnosticCheckpoint `json:"pg_checkpointer_delta"`
	WAL        diagnosticWAL        `json:"pg_wal_delta"`
	Runtime    diagnosticRuntime    `json:"runtime_delta"`
	PGError    string               `json:"pg_error,omitempty"`
}

type diagnosticPool struct {
	Open           int   `json:"open"`
	InUse          int   `json:"in_use"`
	Idle           int   `json:"idle"`
	MaxOpen        int   `json:"max_open"`
	WaitCount      int64 `json:"wait_count_delta"`
	WaitDurationUS int64 `json:"wait_duration_us_delta"`
}

type diagnosticWait struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Count int64  `json:"count"`
}

// PostgreSQL 17 moved checkpoint counters to pg_stat_checkpointer. These are
// sampled as deltas; no SQL text, data, or connection details are recorded.
type diagnosticCheckpoint struct {
	Timed          int64   `json:"timed_delta"`
	Requested      int64   `json:"requested_delta"`
	WriteMS        float64 `json:"write_ms_delta"`
	SyncMS         float64 `json:"sync_ms_delta"`
	BuffersWritten int64   `json:"buffers_written_delta"`
}

type diagnosticWAL struct {
	Records     int64 `json:"records_delta"`
	FullPage    int64 `json:"full_page_images_delta"`
	Bytes       int64 `json:"bytes_delta"`
	BuffersFull int64 `json:"buffers_full_delta"`
	Writes      int64 `json:"writes_delta"`
	Syncs       int64 `json:"syncs_delta"`
}

type diagnosticRuntime struct {
	GCPauseCount uint64  `json:"gc_pause_count_delta"`
	GCPauseMS    float64 `json:"gc_pause_ms_delta"`
	HeapBytes    int64   `json:"heap_bytes_delta"`
	Goroutines   int64   `json:"goroutines_delta"`
}

type diagnosticSampler struct {
	db             *pgstore.DB
	raw            *sql.DB
	started        time.Time
	cancel         context.CancelFunc
	done           chan struct{}
	mu             sync.Mutex
	report         diagnosticReport
	prevPool       sql.DBStats
	prevCheckpoint diagnosticCheckpoint
	prevWAL        diagnosticWAL
	prevRuntime    runtimeCounters
}

type runtimeCounters struct {
	gcPauses   uint64
	gcPauseMS  float64
	heapBytes  uint64
	goroutines uint64
}

func main() {
	os.Exit(runMain(os.Args[1:], os.Getenv, os.Stderr))
}

func runMain(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := parseFlags(args, getenv)
	if err != nil {
		writeRunnerFailure(stderr, runnerFailure{stage: "arguments"})
		return 2
	}
	return runWithStderr(cfg, stderr)
}

func runWithStderr(cfg config, stderr io.Writer) int {
	if err := run(cfg); err != nil {
		writeRunnerFailure(stderr, err)
		return 1
	}
	return 0
}

func writeRunnerFailure(stderr io.Writer, err error) {
	fmt.Fprintln(stderr, "memory-gate-load:", safeRunnerError(err))
}

func parseFlags(args []string, getenv func(string) string) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("memory-gate-load", flag.ContinueOnError)
	// Parse errors and --help must never print flag defaults: a DSN supplied by
	// POSTGRES_TEST_DSN may contain credentials. The public entry point emits a
	// fixed safe stage code after parsing fails.
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.DSN, "dsn", "", "isolated PostgreSQL DSN")
	flags.StringVar(&cfg.ExpectedDatabase, "expected-database", "", "required current_database() value; must start with memory_gate_")
	flags.StringVar(&cfg.Confirm, "confirm", "", "required destructive confirmation: "+confirmation)
	flags.StringVar(&cfg.Output, "output", "", "required JSON artifact path")
	flags.StringVar(&cfg.BuildCommit, "build-commit", "", "required commit embedded in the formal runner binary")
	flags.StringVar(&cfg.RunnerSourceHash, "runner-source-sha256", "", "required runner source hash embedded in the formal binary")
	flags.StringVar(&cfg.ProductHash, "product-sha256", "", "required product source hash embedded in the formal binary")
	flags.StringVar(&cfg.PGImage, "pg-image", "", "required PostgreSQL image reference for formal runs")
	flags.StringVar(&cfg.PGImageDigest, "pg-image-digest", "", "required immutable PostgreSQL image digest for formal runs")
	flags.StringVar(&cfg.PGImageID, "pg-image-id", "", "required Docker-inspected PostgreSQL image ID for formal runs")
	flags.StringVar(&cfg.PGContainer, "pg-container", "", "required disposable PostgreSQL container for formal runs")
	flags.BoolVar(&cfg.ReferenceProfile, "reference-profile", false, "require and record the frozen durable PostgreSQL 17 reference profile")
	flags.IntVar(&cfg.Rows, "rows", 1_000_000, "dataset rows (formal protocol requires 1,000,000)")
	flags.IntVar(&cfg.RPS, "rps", 100, "open-loop scheduled requests per second")
	flags.IntVar(&cfg.MaxInflight, "max-inflight", 32, "non-blocking in-flight limit")
	flags.DurationVar(&cfg.Warmup, "warmup", 2*time.Minute, "warmup duration")
	flags.DurationVar(&cfg.Duration, "duration", 10*time.Minute, "measured duration per scenario")
	flags.StringVar(&cfg.Scenario, "scenario", "all", "recall, mixed, or all")
	flags.BoolVar(&cfg.Smoke, "smoke", false, "permit a non-formal smaller protocol, always labeled formal=false")
	flags.BoolVar(&cfg.DiagnosticTelemetry, "diagnostic-telemetry", false, "write bounded timing-only non-formal diagnostic telemetry")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.DSN == "" {
		cfg.DSN = getenv("POSTGRES_TEST_DSN")
	}
	return cfg, nil
}

func run(cfg config) error {
	if cfg.DSN == "" || cfg.ExpectedDatabase == "" || cfg.Output == "" {
		return preflightFailure("preflight_config", errors.New("dsn, expected-database, and output are required"))
	}
	if cfg.Confirm != confirmation {
		return preflightFailure("preflight_config", fmt.Errorf("refusing destructive seed without --confirm %s", confirmation))
	}
	if !strings.HasPrefix(cfg.ExpectedDatabase, "memory_gate_") {
		return preflightFailure("preflight_config", errors.New("expected database must start with memory_gate_"))
	}
	if cfg.Rows <= 0 || cfg.Rows%1000 != 0 || cfg.RPS <= 0 || cfg.MaxInflight <= 0 || cfg.Duration <= 0 || cfg.Warmup < 0 {
		return preflightFailure("preflight_config", errors.New("rows must be a positive multiple of 1000 and load bounds must be positive"))
	}
	if cfg.Scenario != "recall" && cfg.Scenario != "mixed" && cfg.Scenario != "all" {
		return preflightFailure("preflight_config", errors.New("scenario must be recall, mixed, or all"))
	}
	formal := cfg.Rows == 1_000_000 && cfg.RPS == 100 && cfg.MaxInflight == 32 && cfg.Warmup == 2*time.Minute && cfg.Duration == 10*time.Minute && cfg.Scenario == "all" && !cfg.Smoke
	if !cfg.Smoke && !formal {
		return preflightFailure("preflight_config", errors.New("non-formal bounds require --smoke; formal bounds are fixed and cannot be weakened"))
	}
	if cfg.DiagnosticTelemetry && formal {
		return preflightFailure("preflight_config", errors.New("diagnostic telemetry is non-formal only; use --smoke for an explicitly unscoreable diagnostic run"))
	}
	if formal && (cfg.PGImage == "" || cfg.PGImageDigest == "" || cfg.PGImageID == "" || cfg.PGContainer == "" || cfg.BuildCommit == "" || cfg.RunnerSourceHash == "" || cfg.ProductHash == "" || !cfg.ReferenceProfile) {
		return preflightFailure("preflight_config", errors.New("formal runs require build binding and pg image/digest/id/container"))
	}
	if err := validateAuditFields(cfg); err != nil {
		return preflightFailure("preflight_audit", err)
	}
	var endpoint dsnEndpoint
	var err error
	if formal || cfg.ReferenceProfile {
		endpoint, err = parseFormalDSN(cfg.DSN)
		if err != nil {
			return preflightFailure("preflight_dsn", err)
		}
	}
	cfg.RedactedDSN = redactDSN(cfg.DSN)
	proof, err := collectProvenance()
	if err != nil {
		return preflightFailure("preflight_provenance", err)
	}
	proof.Command = redactCommand(os.Args)
	if formal || cfg.ReferenceProfile {
		if err := validateBuildBinding(cfg, proof); err != nil {
			return preflightFailure("preflight_build", err)
		}
	}
	if err := writeInvocation(cfg.Output, proof); err != nil {
		return preflightFailure("preflight_invocation", err)
	}

	ctx := context.Background()
	raw, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return safeDatabaseError("database_open", err)
	}
	defer raw.Close()
	var database string
	if err := raw.QueryRowContext(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return safeDatabaseError("database_connect", err)
	}
	if database != cfg.ExpectedDatabase {
		return preflightFailure("preflight_database_identity", fmt.Errorf("current database %q does not match explicit disposable database %q", database, cfg.ExpectedDatabase))
	}
	var dockerImage dockerImageProvenance
	if formal || cfg.ReferenceProfile {
		dockerImage, err = inspectDockerImage(cfg, endpoint)
		if err != nil {
			return preflightFailure("preflight_docker", err)
		}
	}

	db, err := pgstore.New(ctx, cfg.DSN, pgstore.Options{SkipSchemaCheck: true, MaxOpenConns: cfg.MaxInflight + 8, MaxIdleConns: cfg.MaxInflight + 8})
	if err != nil {
		return safeDatabaseError("database_product_pool", err)
	}
	defer db.Close()
	limits := memory.DefaultLimits()
	limits.MaxRecordsPerScope = 2_000
	limits.MaxContentPerScope = 8 << 20
	store := pgstore.NewMemoryStore(db).SetLimits(limits)
	var moduleStore memory.AtomicStore = store
	var recallObserver memory.RecallObserver
	if cfg.DiagnosticTelemetry {
		moduleStore = telemetryStore{AtomicStore: store}
		recallObserver = runnerRecallObserver{}
	}
	module, err := newModule(moduleStore, limits, recallObserver)
	if err != nil {
		return preflightFailure("preflight_module", err)
	}

	out := report{
		Protocol: "docs/memory/design.md#8", Formal: formal, Seed: gateSeed, Config: cfg,
		Provenance:    proof,
		Environment:   map[string]any{"go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH, "logical_cpus": runtime.NumCPU()},
		Thresholds:    map[string]string{"recall": "p95<=20ms p99<=50ms success>=99.9%", "write": "p95<=50ms p99<=100ms", "safety": "CAS winners=1; remaining contenders conflict or bounded-overload; quota successes=0"},
		ThresholdPass: make(map[string]bool),
	}
	if formal || cfg.ReferenceProfile {
		out.Connection = connectionProvenance{Host: endpoint.Host, Port: endpoint.Port, Database: endpoint.Database, User: endpoint.User}
		out.DockerImage = dockerImage
	}
	datasetIdentity, _ := json.Marshal(struct {
		Version string `json:"version"`
		Seed    int64  `json:"seed"`
		Rows    int    `json:"rows"`
	}{datasetVersion, gateSeed, cfg.Rows})
	sum := sha256.Sum256(datasetIdentity)
	out.DatasetSHA256 = hex.EncodeToString(sum[:])
	out.PostgreSQL, err = db.RuntimeSettings(ctx, "server_version", "fsync", "synchronous_commit", "full_page_writes", "shared_buffers", "max_connections", "checkpoint_timeout", "checkpoint_completion_target", "max_wal_size", "min_wal_size", "wal_buffers", "autovacuum")
	if err != nil {
		return safeDatabaseError("database_settings", err)
	}
	if formal || cfg.ReferenceProfile {
		profile, err := validateReferenceProfile(cfg, out.PostgreSQL, dockerImage)
		if err != nil {
			return preflightFailure("preflight_durability", err)
		}
		out.ReferenceProfile = &profile
	}
	if err := db.Migrate(ctx); err != nil {
		return safeDatabaseError("database_migrate", err)
	}
	out.PreSeedCounts, err = readCounts(ctx, raw)
	if err != nil {
		return safeDatabaseError("database_preseed_counts", err)
	}
	out.SeedStartedAt = time.Now().UTC()
	if err := seedDataset(ctx, raw, limits, cfg.Rows); err != nil {
		return safeDatabaseError("database_seed", err)
	}
	out.SeedFinishedAt = time.Now().UTC()
	out.PostSeedCounts, err = readCounts(ctx, raw)
	if err != nil {
		return safeDatabaseError("database_postseed_counts", err)
	}
	if cfg.ReferenceProfile {
		if _, err := raw.ExecContext(ctx, `ANALYZE`); err != nil {
			return safeDatabaseError("database_analyze", err)
		}
		if _, err := raw.ExecContext(ctx, `CHECKPOINT`); err != nil {
			return safeDatabaseError("database_checkpoint", err)
		}
	}
	out.QueryPlan, err = queryPlan(ctx, raw)
	if err != nil {
		return safeDatabaseError("database_query_plan", err)
	}
	var sampler *diagnosticSampler
	if cfg.DiagnosticTelemetry {
		sampler = startDiagnosticSampler(ctx, db, raw)
	}

	scenarios := []string{cfg.Scenario}
	if cfg.Scenario == "all" {
		scenarios = []string{"recall", "mixed"}
	}
	var referenceProfileErr error
	for scenarioIndex, name := range scenarios {
		base := int64(scenarioIndex) * 1_000_000
		warmupAttempts := int64(cfg.Warmup) * int64(cfg.RPS) / int64(time.Second)
		if cfg.Warmup > 0 {
			if _, err := runScenario(ctx, module, name, cfg.Warmup, cfg.RPS, cfg.MaxInflight, cfg.Rows/1000, base, false, cfg.DiagnosticTelemetry); err != nil {
				if sampler != nil {
					diagnostics := sampler.stop()
					out.Diagnostics = &diagnostics
				}
				return safeDatabaseError("database_warmup", err)
			}
		}
		var checkpointStart checkpointCounters
		if cfg.ReferenceProfile {
			checkpointStart, err = readCheckpointCounters(ctx, raw)
			if err != nil {
				return safeDatabaseError("database_checkpoint_start", err)
			}
		}
		scenario, err := runScenario(ctx, module, name, cfg.Duration, cfg.RPS, cfg.MaxInflight, cfg.Rows/1000, base+warmupAttempts, true, cfg.DiagnosticTelemetry)
		if err != nil {
			if sampler != nil {
				diagnostics := sampler.stop()
				out.Diagnostics = &diagnostics
			}
			return safeDatabaseError("database_load", err)
		}
		if cfg.ReferenceProfile {
			checkpointEnd, err := readCheckpointCounters(ctx, raw)
			if err != nil {
				return safeDatabaseError("database_checkpoint_end", err)
			}
			scenario.Checkpoint = &scenarioCheckpointEvidence{Start: checkpointStart, End: checkpointEnd, Completed: (checkpointEnd.Timed - checkpointStart.Timed) + (checkpointEnd.Requested - checkpointStart.Requested)}
			if formal && scenario.Checkpoint.Completed < 1 {
				referenceProfileErr = fmt.Errorf("formal scenario %s completed no checkpoint", name)
			}
		}
		out.Scenarios = append(out.Scenarios, scenario)
	}
	if sampler != nil {
		diagnostics := sampler.stop()
		out.Diagnostics = &diagnostics
	}
	out.Correctness, err = runCorrectness(ctx, raw, module, store, limits)
	if err != nil {
		return safeDatabaseError("database_correctness", err)
	}
	out.PostRunCounts, err = readCounts(ctx, raw)
	if err != nil {
		return safeDatabaseError("database_postrun_counts", err)
	}
	evaluationErr := evaluateThresholds(&out)
	if referenceProfileErr != nil {
		out.ThresholdPass["reference_profile"] = false
		if evaluationErr == nil {
			evaluationErr = referenceProfileErr
		}
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(cfg.Output, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	if evaluationErr != nil {
		return fmt.Errorf("formal result accounting: %w; complete raw report saved to %s", evaluationErr, cfg.Output)
	}
	for key, passed := range out.ThresholdPass {
		if !passed {
			return fmt.Errorf("threshold %s failed; complete raw report saved to %s", key, cfg.Output)
		}
	}
	return nil
}

func readCheckpointCounters(ctx context.Context, db *sql.DB) (checkpointCounters, error) {
	var counters checkpointCounters
	err := db.QueryRowContext(ctx, `SELECT num_timed,num_requested FROM pg_stat_checkpointer`).Scan(&counters.Timed, &counters.Requested)
	return counters, err
}

func safeDatabaseError(stage string, err error) error {
	// pgx ParseConfigError and connection failures can include the original DSN
	// or query text. Do not wrap them: benchmark stderr and sidecars must never
	// disclose credentials or attacker-controlled connection parameters.
	if err == nil {
		return nil
	}
	return runnerFailure{stage: stage}
}

func safeRunnerError(err error) error {
	var failure runnerFailure
	if errors.As(err, &failure) {
		return failure
	}
	// The final stderr boundary never renders an arbitrary error chain. This
	// also protects future call sites that accidentally return driver errors.
	return runnerFailure{stage: "runner"}
}

func startDiagnosticSampler(parent context.Context, db *pgstore.DB, raw *sql.DB) *diagnosticSampler {
	ctx, cancel := context.WithCancel(parent)
	sampler := &diagnosticSampler{
		db: db, raw: raw, started: time.Now(), cancel: cancel, done: make(chan struct{}),
		report: diagnosticReport{IntervalMS: 1_000, SampleLimit: diagnosticSampleLimit},
	}
	go func() {
		defer close(sampler.done)
		sampler.sample(ctx)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sampler.sample(ctx)
			}
		}
	}()
	return sampler
}

func (s *diagnosticSampler) stop() diagnosticReport {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.report
	out.Samples = append([]diagnosticSample(nil), s.report.Samples...)
	return out
}

func (s *diagnosticSampler) sample(parent context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.report.Samples) >= diagnosticSampleLimit {
		s.report.DroppedSamples++
		return
	}
	pool := s.db.PoolStats()
	currentRuntime := readRuntimeCounters()
	sample := diagnosticSample{
		OffsetMS: time.Since(s.started).Milliseconds(),
		Pool: diagnosticPool{Open: pool.OpenConnections, InUse: pool.InUse, Idle: pool.Idle, MaxOpen: pool.MaxOpenConnections,
			WaitCount: pool.WaitCount - s.prevPool.WaitCount, WaitDurationUS: (pool.WaitDuration - s.prevPool.WaitDuration).Microseconds()},
		Runtime: runtimeDelta(currentRuntime, s.prevRuntime),
	}
	s.prevPool = pool
	s.prevRuntime = currentRuntime
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	checkpoint, wal, waits, err := readPostgreSQLDiagnostics(ctx, s.raw)
	if err != nil {
		// Keep a bounded stage marker, never a driver error that may carry a
		// connection string or query text.
		sample.PGError = "stats_unavailable"
	} else {
		sample.Checkpoint = checkpointDelta(checkpoint, s.prevCheckpoint)
		sample.WAL = walDelta(wal, s.prevWAL)
		sample.Waits = waits
		s.prevCheckpoint = checkpoint
		s.prevWAL = wal
	}
	s.report.Samples = append(s.report.Samples, sample)
}

func readPostgreSQLDiagnostics(ctx context.Context, db *sql.DB) (diagnosticCheckpoint, diagnosticWAL, []diagnosticWait, error) {
	var checkpoint diagnosticCheckpoint
	if err := db.QueryRowContext(ctx, `SELECT num_timed,num_requested,write_time,sync_time,buffers_written FROM pg_stat_checkpointer`).Scan(&checkpoint.Timed, &checkpoint.Requested, &checkpoint.WriteMS, &checkpoint.SyncMS, &checkpoint.BuffersWritten); err != nil {
		return diagnosticCheckpoint{}, diagnosticWAL{}, nil, err
	}
	var wal diagnosticWAL
	if err := db.QueryRowContext(ctx, `SELECT wal_records,wal_fpi,wal_bytes,wal_buffers_full,wal_write,wal_sync FROM pg_stat_wal`).Scan(&wal.Records, &wal.FullPage, &wal.Bytes, &wal.BuffersFull, &wal.Writes, &wal.Syncs); err != nil {
		return diagnosticCheckpoint{}, diagnosticWAL{}, nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(wait_event_type,''),COALESCE(wait_event,''),count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() GROUP BY 1,2 ORDER BY 1,2`)
	if err != nil {
		return diagnosticCheckpoint{}, diagnosticWAL{}, nil, err
	}
	defer rows.Close()
	var waits []diagnosticWait
	for rows.Next() {
		var wait diagnosticWait
		if err := rows.Scan(&wait.Type, &wait.Event, &wait.Count); err != nil {
			return diagnosticCheckpoint{}, diagnosticWAL{}, nil, err
		}
		waits = append(waits, wait)
	}
	return checkpoint, wal, waits, rows.Err()
}

func checkpointDelta(now, previous diagnosticCheckpoint) diagnosticCheckpoint {
	return diagnosticCheckpoint{Timed: now.Timed - previous.Timed, Requested: now.Requested - previous.Requested, WriteMS: now.WriteMS - previous.WriteMS, SyncMS: now.SyncMS - previous.SyncMS, BuffersWritten: now.BuffersWritten - previous.BuffersWritten}
}

func walDelta(now, previous diagnosticWAL) diagnosticWAL {
	return diagnosticWAL{Records: now.Records - previous.Records, FullPage: now.FullPage - previous.FullPage, Bytes: now.Bytes - previous.Bytes, BuffersFull: now.BuffersFull - previous.BuffersFull, Writes: now.Writes - previous.Writes, Syncs: now.Syncs - previous.Syncs}
}

func readRuntimeCounters() runtimeCounters {
	samples := []runtimeMetrics.Sample{{Name: "/gc/pauses:seconds"}, {Name: "/memory/classes/heap/objects:bytes"}, {Name: "/sched/goroutines:goroutines"}}
	runtimeMetrics.Read(samples)
	counters := runtimeCounters{heapBytes: samples[1].Value.Uint64(), goroutines: samples[2].Value.Uint64()}
	histogram := samples[0].Value.Float64Histogram()
	counters.gcPauses, counters.gcPauseMS = histogramPauseCounters(histogram.Buckets, histogram.Counts)
	return counters
}

func histogramPauseCounters(buckets []float64, counts []uint64) (uint64, float64) {
	var total uint64
	var milliseconds float64
	for index, count := range counts {
		total += count
		if index+1 < len(buckets) && !math.IsInf(buckets[index+1], 0) {
			milliseconds += float64(count) * buckets[index+1] * 1_000
		}
	}
	return total, milliseconds
}

func runtimeDelta(now, previous runtimeCounters) diagnosticRuntime {
	return diagnosticRuntime{GCPauseCount: now.gcPauses - previous.gcPauses, GCPauseMS: now.gcPauseMS - previous.gcPauseMS, HeapBytes: int64(now.heapBytes) - int64(previous.heapBytes), Goroutines: int64(now.goroutines) - int64(previous.goroutines)}
}

func newModule(store memory.AtomicStore, limits memory.Limits, observer memory.RecallObserver) (*memory.Module, error) {
	return memory.NewModule(memory.ModuleConfig{
		Store: store, Authorizer: memory.AuthorizerFunc(func(context.Context, memory.AuthorizeRequest) error { return nil }),
		Keys: map[string][]byte{"gate": []byte("0123456789abcdef0123456789abcdef")}, CurrentKey: "gate", Limits: limits, RecallObserver: observer,
	})
}

func seedDataset(ctx context.Context, db *sql.DB, limits memory.Limits, rows int) error {
	if _, err := db.ExecContext(ctx, `TRUNCATE agent_runtime_memory_audits,agent_runtime_memory_receipts,agent_runtime_memory_candidate_batches,agent_runtime_memory_sources,agent_runtime_memory_records,agent_runtime_memory_lifecycle_jobs,agent_runtime_memory_purge_jobs,agent_runtime_memory_collections,agent_runtime_memory_subjects,agent_runtime_memory_policies CASCADE`); err != nil {
		return fmt.Errorf("truncate isolated gate tables: %w", err)
	}
	rawLimits, err := json.Marshal(limits)
	if err != nil {
		return err
	}
	subjects := rows / 1000
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits)
		SELECT 'tenant-'||lpad(t::text,3,'0'),'project',1,$1::jsonb FROM generate_series(0,$2-1) t`, rawLimits, (subjects+9)/10); err != nil {
		return fmt.Errorf("seed policies: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled)
		SELECT 'tenant-'||lpad((s/10)::text,3,'0'),'project','subject-'||lpad(s::text,4,'0'),1,false FROM generate_series(0,$1-1) s`, subjects); err != nil {
		return fmt.Errorf("seed subjects: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id,records_count,content_bytes,record_identities,candidate_batches,proposal_epoch)
		SELECT 'tenant-'||lpad((s/10)::text,3,'0'),'project','subject-'||lpad(s::text,4,'0'),'profile',1000,512000,1000,0,0 FROM generate_series(0,$1-1) s`, subjects); err != nil {
		return fmt.Errorf("seed collections: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,proposal_epoch,kind,content,topics,status,source_ref,source_version,source_epoch,producer,policy_version,decision_actor,decision_basis,decided_at,created_at,updated_at,expires_at)
		SELECT 'tenant-'||lpad(((g/1000)/10)::text,3,'0'),'project','subject-'||lpad((g/1000)::text,4,'0'),'profile',
		'memory-'||lpad(g::text,7,'0'),1,1,1,0,'preference',
		rpad('active-token subject-'||lpad((g/1000)::text,4,'0')||' row-'||g::text,CASE WHEN g%4=0 THEN 256 WHEN g%4=3 THEN 768 ELSE 512 END,'x'),
		'[]'::jsonb,'active','','',0,'mem008-gate','v1','','',NULL,transaction_timestamp(),
		transaction_timestamp()-(g%1000||' microseconds')::interval,
		CASE WHEN g%1000>=950 THEN transaction_timestamp()-interval '1 hour' ELSE transaction_timestamp()+interval '24 hours' END
		FROM generate_series(0,$1-1) g`, rows); err != nil {
		return fmt.Errorf("seed records: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ANALYZE agent_runtime_memory_records`); err != nil {
		return fmt.Errorf("analyze records: %w", err)
	}
	return nil
}

func runScenario(ctx context.Context, module *memory.Module, name string, duration time.Duration, rps, maxInflight, subjectCount int, sequenceOffset int64, retain, diagnostic bool) (scenarioReport, error) {
	start := time.Now()
	report := scenarioReport{Name: name, StartedAt: start.UTC()}
	interval := time.Second / time.Duration(rps)
	deadline := start.Add(duration)
	sem := make(chan struct{}, maxInflight)
	var wg sync.WaitGroup
	var collector attemptCollector
	var firstErr atomic.Pointer[error]
	var sequence int64
	for scheduled := start; scheduled.Before(deadline); scheduled = scheduled.Add(interval) {
		if wait := time.Until(scheduled); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return report, ctx.Err()
			case <-timer.C:
			}
		}
		seq := sequenceOffset + sequence
		sequence++
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func(scheduled time.Time, seq int64) {
				defer wg.Done()
				defer func() { <-sem }()
				a := executeAttempt(module, name, start, scheduled, seq, subjectCount, diagnostic)
				if retain {
					collector.append(a)
				}
			}(scheduled, seq)
		default:
			if retain {
				now := time.Now()
				collector.append(attempt{Sequence: seq, Scenario: name, Operation: scheduledOperation(name, seq), ScheduledOffsetUS: scheduled.Sub(start).Microseconds(), StartOffsetUS: now.Sub(start).Microseconds(), EndOffsetUS: now.Sub(start).Microseconds(), LatencyUS: max(0, now.Sub(scheduled).Microseconds()), QueueDelayUS: max(0, now.Sub(scheduled).Microseconds()), Status: "generator_overload"})
			}
		}
	}
	wg.Wait()
	report.Attempts = collector.snapshot()
	if ptr := firstErr.Load(); ptr != nil {
		return report, *ptr
	}
	report.FinishedAt = time.Now().UTC()
	sort.Slice(report.Attempts, func(i, j int) bool { return report.Attempts[i].Sequence < report.Attempts[j].Sequence })
	report.ByOperation = summarize(report.Attempts)
	return report, nil
}

func executeAttempt(module *memory.Module, scenario string, origin, scheduled time.Time, seq int64, subjectCount int, diagnostic bool) attempt {
	started := time.Now()
	op := scheduledOperation(scenario, seq)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var trace *attemptTrace
	if diagnostic {
		trace = &attemptTrace{}
		ctx = context.WithValue(ctx, attemptTraceKey{}, trace)
	}
	subjectIndex, localIndex := loadTarget(scenario, seq, op, subjectCount)
	scope := memory.Scope{Namespace: fmt.Sprintf("tenant-%03d", subjectIndex/10), ProjectID: "project", SubjectID: fmt.Sprintf("subject-%04d", subjectIndex), CollectionID: "profile"}
	principal := memory.Principal{Subject: scope.SubjectID, Namespace: scope.Namespace, Issuer: "mem008-load"}
	var err error
	switch op {
	case "recall":
		query := "active-token"
		if seq%20 == 0 {
			query = "nonexistent-query"
		} else if seq%20 == 1 {
			query = "expired-only-query"
		}
		_, err = module.Recall(ctx, principal, scope, memory.RecallRequest{Query: query, Limit: 10})
	case "create":
		var token string
		token, err = module.IssueOperationToken(ctx, principal, scope, memory.ActionCreate, time.Hour)
		if err == nil {
			body := loadBody(seq)
			_, err = module.Create(ctx, principal, scope, memory.CreateRequest{Content: body, Kind: memory.KindPreference, Retention: time.Hour, OperationToken: token})
		}
	case "revise":
		var token string
		token, err = module.IssueOperationToken(ctx, principal, scope, memory.ActionRevise, time.Hour)
		if err == nil {
			_, err = module.Revise(ctx, principal, scope, memory.ReviseRequest{MemoryID: fmt.Sprintf("memory-%07d", subjectIndex*1000+localIndex), ExpectedRevision: 1, Content: loadBody(seq), Retention: time.Hour, OperationToken: token})
		}
	}
	ended := time.Now()
	status := classify(err)
	latency := ended.Sub(scheduled)
	if errors.Is(err, context.DeadlineExceeded) && latency < time.Second {
		latency = time.Second
	}
	item := attempt{Sequence: seq, Scenario: scenario, Operation: op, ScheduledOffsetUS: scheduled.Sub(origin).Microseconds(), StartOffsetUS: started.Sub(origin).Microseconds(), EndOffsetUS: ended.Sub(origin).Microseconds(), LatencyUS: latency.Microseconds(), QueueDelayUS: max(0, started.Sub(scheduled).Microseconds()), OperationUS: ended.Sub(started).Microseconds(), Status: status}
	if trace != nil {
		item.TokenSnapshotUS = trace.tokenSnapshotUS
		item.DBListRecordsUS = trace.dbListRecordsUS
		item.RankingUS = trace.rankingUS
		item.DeadlineCause = trace.deadlineCause
	}
	return item
}

func scheduledOperation(scenario string, seq int64) string {
	if scenario != "mixed" || seq%10 < 8 {
		return "recall"
	}
	if seq%10 == 8 {
		return "create"
	}
	return "revise"
}

func loadTarget(scenario string, seq int64, op string, subjectCount int) (subject, local int) {
	if scenario == "mixed" && op == "revise" {
		writeSeq := seq / 10
		return int(writeSeq % int64(subjectCount)), int((writeSeq / int64(subjectCount)) % 950)
	}
	return int((seq*7919 + gateSeed) % int64(subjectCount)), int(seq % 950)
}

func loadBody(seq int64) string {
	size := []int{256, 512, 512, 768}[seq%4]
	prefix := fmt.Sprintf("mem008 write %d ", seq)
	return prefix + strings.Repeat("w", size-len(prefix))
}

func classify(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, memory.ErrOverloaded) {
		return "module_overload"
	}
	if errors.Is(err, memory.ErrConflict) {
		return "conflict"
	}
	if errors.Is(err, memory.ErrQuota) {
		return "quota"
	}
	return "error"
}

func summarize(attempts []attempt) map[string]metrics {
	grouped := make(map[string][]attempt)
	for _, item := range attempts {
		grouped[item.Operation] = append(grouped[item.Operation], item)
	}
	out := make(map[string]metrics, len(grouped))
	for op, items := range grouped {
		latencies := make([]int64, len(items))
		m := metrics{Attempts: len(items)}
		for i, item := range items {
			latencies[i] = item.LatencyUS
			switch item.Status {
			case "success":
				m.Successes++
			case "generator_overload", "module_overload":
				m.Overloads++
			case "timeout":
				m.Timeouts++
			default:
				m.Errors++
			}
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		if len(items) > 0 {
			m.SuccessRate = float64(m.Successes) / float64(len(items))
			m.P50US, m.P95US, m.P99US, m.MaxUS = percentile(latencies, 50), percentile(latencies, 95), percentile(latencies, 99), latencies[len(latencies)-1]
		}
		out[op] = m
	}
	return out
}

func percentile(sorted []int64, pct int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*pct + 99) / 100
	if index < 1 {
		index = 1
	}
	return sorted[index-1]
}

func runCorrectness(ctx context.Context, db *sql.DB, module *memory.Module, store *pgstore.MemoryStore, limits memory.Limits) (correctnessReport, error) {
	var out correctnessReport
	scope := memory.Scope{Namespace: "tenant-000", ProjectID: "project", SubjectID: "subject-0000", CollectionID: "profile"}
	principal := memory.Principal{Subject: scope.SubjectID, Namespace: scope.Namespace, Issuer: "mem008-load"}
	record, _, err := store.ReadRecord(ctx, scope, "memory-0000000")
	if err != nil || record == nil {
		return out, fmt.Errorf("read CAS fixture: %w", err)
	}
	tokens := make([]string, 64)
	for i := range tokens {
		tokens[i], err = module.IssueOperationToken(ctx, principal, scope, memory.ActionRevise, time.Hour)
		if err != nil {
			return out, err
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 64)
	for i := range tokens {
		go func(i int) {
			<-start
			_, err := module.Revise(context.Background(), principal, scope, memory.ReviseRequest{MemoryID: record.MemoryID, ExpectedRevision: record.Revision, Content: fmt.Sprintf("CAS contender %02d", i), Retention: time.Hour, OperationToken: tokens[i]})
			errs <- err
		}(i)
	}
	close(start)
	for range tokens {
		err := <-errs
		if err == nil {
			out.CASWinners++
		} else if errors.Is(err, memory.ErrConflict) {
			out.CASConflicts++
		} else if errors.Is(err, memory.ErrOverloaded) {
			out.CASOverloads++
		} else {
			out.CASOtherErrors++
		}
	}

	quotaLimits := limits
	quotaLimits.MaxRecordsPerScope = 1
	rawLimits, _ := json.Marshal(quotaLimits)
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_policies(namespace,project_id,generation,limits) VALUES('memory-gate-quota','project',1,$1::jsonb)`, rawLimits); err != nil {
		return out, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_subjects(namespace,project_id,subject_id,generation,disabled) VALUES('memory-gate-quota','project','subject',1,false)`); err != nil {
		return out, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_collections(namespace,project_id,subject_id,collection_id,records_count,content_bytes,record_identities,candidate_batches,proposal_epoch) VALUES('memory-gate-quota','project','subject','profile',1,4,1,0,0)`); err != nil {
		return out, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_runtime_memory_records(namespace,project_id,subject_id,collection_id,memory_id,revision,subject_generation,policy_generation,proposal_epoch,kind,content,topics,status,source_ref,source_version,source_epoch,producer,policy_version,decision_actor,decision_basis,decided_at,created_at,updated_at,expires_at) VALUES('memory-gate-quota','project','subject','profile','existing',1,1,1,0,'preference','full','[]','active','','',0,'gate','v1','','',NULL,transaction_timestamp(),transaction_timestamp(),transaction_timestamp()+interval '1 hour')`); err != nil {
		return out, err
	}
	quotaScope := memory.Scope{Namespace: "memory-gate-quota", ProjectID: "project", SubjectID: "subject", CollectionID: "profile"}
	quotaPrincipal := memory.Principal{Subject: "subject", Namespace: "memory-gate-quota", Issuer: "mem008-load"}
	for i := 0; i < 64; i++ {
		token, err := module.IssueOperationToken(ctx, quotaPrincipal, quotaScope, memory.ActionCreate, time.Hour)
		if err != nil {
			return out, err
		}
		_, err = module.Create(ctx, quotaPrincipal, quotaScope, memory.CreateRequest{Content: fmt.Sprintf("quota contender %02d", i), Kind: memory.KindPreference, Retention: time.Hour, OperationToken: token})
		if err == nil {
			out.QuotaSuccesses++
		} else if errors.Is(err, memory.ErrQuota) {
			out.QuotaRejections++
		} else {
			out.QuotaOtherErrors++
		}
	}
	return out, nil
}

func validateFormalAccounting(out *report) error {
	expected := map[string]map[string]int{
		"recall": {"recall": 60_000},
		"mixed":  {"recall": 48_000, "create": 6_000, "revise": 6_000},
	}
	ranges := map[string]struct{ first, last int64 }{
		"recall": {first: 12_000, last: 71_999},
		"mixed":  {first: 1_012_000, last: 1_071_999},
	}
	if len(out.Scenarios) != len(expected) {
		return fmt.Errorf("formal result has %d scenarios, want %d", len(out.Scenarios), len(expected))
	}
	sequences := make(map[int64]struct{}, 120_000)
	seenScenarios := make(map[string]struct{}, len(expected))
	total := 0
	for _, scenario := range out.Scenarios {
		operations, ok := expected[scenario.Name]
		if !ok {
			return fmt.Errorf("unexpected formal scenario %q", scenario.Name)
		}
		if _, duplicate := seenScenarios[scenario.Name]; duplicate {
			return fmt.Errorf("duplicate formal scenario %q", scenario.Name)
		}
		seenScenarios[scenario.Name] = struct{}{}
		if len(scenario.ByOperation) != len(operations) {
			return fmt.Errorf("formal %s has %d operation summaries, want %d", scenario.Name, len(scenario.ByOperation), len(operations))
		}
		computed := summarize(scenario.Attempts)
		for operation, wantAttempts := range operations {
			got, ok := scenario.ByOperation[operation]
			if !ok {
				return fmt.Errorf("formal %s is missing %s summary", scenario.Name, operation)
			}
			if got.Attempts != wantAttempts {
				return fmt.Errorf("formal %s/%s has %d attempts, want %d", scenario.Name, operation, got.Attempts, wantAttempts)
			}
			if got.Successes+got.Errors+got.Overloads+got.Timeouts != got.Attempts {
				return fmt.Errorf("formal %s/%s outcome counts do not equal attempts", scenario.Name, operation)
			}
			if recomputed, ok := computed[operation]; !ok || recomputed != got {
				return fmt.Errorf("formal %s/%s summary does not match complete attempt log", scenario.Name, operation)
			}
		}
		for _, item := range scenario.Attempts {
			if item.Scenario != scenario.Name {
				return fmt.Errorf("attempt sequence %d has scenario %q, want %q", item.Sequence, item.Scenario, scenario.Name)
			}
			if _, ok := operations[item.Operation]; !ok {
				return fmt.Errorf("attempt sequence %d has unexpected operation %q", item.Sequence, item.Operation)
			}
			interval := ranges[scenario.Name]
			if item.Sequence < interval.first || item.Sequence > interval.last {
				return fmt.Errorf("attempt sequence %d is outside formal %s range [%d,%d]", item.Sequence, scenario.Name, interval.first, interval.last)
			}
			if scheduledOperation(scenario.Name, item.Sequence) != item.Operation {
				return fmt.Errorf("attempt sequence %d operation %q violates scheduled operation mapping", item.Sequence, item.Operation)
			}
			if !validAttemptStatus(item.Status) {
				return fmt.Errorf("attempt sequence %d has invalid status %q", item.Sequence, item.Status)
			}
			if item.ScheduledOffsetUS < 0 || item.StartOffsetUS < item.ScheduledOffsetUS || item.EndOffsetUS < item.StartOffsetUS || item.LatencyUS < 0 {
				return fmt.Errorf("attempt sequence %d has invalid timing fields", item.Sequence)
			}
			expectedScheduled := (item.Sequence - interval.first) * 10_000
			if item.ScheduledOffsetUS != expectedScheduled {
				return fmt.Errorf("attempt sequence %d scheduled offset %d, want %d", item.Sequence, item.ScheduledOffsetUS, expectedScheduled)
			}
			elapsed := item.EndOffsetUS - item.ScheduledOffsetUS
			if item.Status == "timeout" {
				if item.LatencyUS < 1_000_000 || item.LatencyUS < elapsed {
					return fmt.Errorf("timeout sequence %d lacks the one-second latency lower bound", item.Sequence)
				}
			} else if item.LatencyUS != elapsed {
				return fmt.Errorf("attempt sequence %d latency %d does not equal end-scheduled %d", item.Sequence, item.LatencyUS, elapsed)
			}
			if _, duplicate := sequences[item.Sequence]; duplicate {
				return fmt.Errorf("duplicate formal attempt sequence %d", item.Sequence)
			}
			sequences[item.Sequence] = struct{}{}
			total++
		}
	}
	if total != 120_000 || len(sequences) != 120_000 {
		return fmt.Errorf("formal attempt log has %d total / %d unique sequences, want 120000 each", total, len(sequences))
	}
	return nil
}

func validAttemptStatus(status string) bool {
	switch status {
	case "success", "timeout", "module_overload", "generator_overload", "conflict", "quota", "error":
		return true
	default:
		return false
	}
}

func evaluateThresholds(out *report) error {
	if out.Formal {
		if err := validateFormalAccounting(out); err != nil {
			out.ThresholdPass["recall"] = false
			out.ThresholdPass["write"] = false
			out.ThresholdPass["safety"] = false
			return err
		}
	}
	recallPass, writePass := true, true
	for _, scenario := range out.Scenarios {
		if m, ok := scenario.ByOperation["recall"]; ok {
			// summarize includes every outcome in Attempts and percentile samples.
			// The frozen Recall contract judges the resulting success rate and
			// end-to-end percentiles; it does not impose a separate zero-timeout
			// condition. Safety failures remain independently zero-tolerant.
			recallPass = recallPass && m.P95US <= 20_000 && m.P99US <= 50_000 && m.SuccessRate >= .999
		}
		for _, op := range []string{"create", "revise"} {
			if m, ok := scenario.ByOperation[op]; ok {
				writePass = writePass && m.P95US <= 50_000 && m.P99US <= 100_000 && m.Errors == 0 && m.Overloads == 0 && m.Timeouts == 0
			}
		}
	}
	out.ThresholdPass["recall"] = recallPass
	out.ThresholdPass["write"] = writePass
	out.ThresholdPass["safety"] = out.Correctness.CASWinners == 1 && out.Correctness.CASConflicts+out.Correctness.CASOverloads == 63 && out.Correctness.CASOtherErrors == 0 && out.Correctness.QuotaSuccesses == 0 && out.Correctness.QuotaRejections == 64 && out.Correctness.QuotaOtherErrors == 0
	return nil
}

func queryPlan(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var raw []byte
	err := db.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON)
		SELECT r.memory_id,r.revision,r.subject_generation,r.policy_generation,r.proposal_epoch,r.kind,r.content,r.topics,r.status,r.source_ref,r.source_version,r.source_epoch,r.producer,r.policy_version,r.decision_actor,r.decision_basis,r.decided_at,r.created_at,r.updated_at,r.expires_at
		FROM agent_runtime_memory_records r
		WHERE r.namespace='tenant-000' AND r.project_id='project' AND r.subject_id='subject-0000' AND r.collection_id='profile'
		AND r.subject_generation=1 AND r.status='active' AND r.content<>'' AND r.expires_at>transaction_timestamp()
		ORDER BY r.updated_at DESC,r.memory_id DESC LIMIT 200`).Scan(&raw)
	return json.RawMessage(raw), err
}
