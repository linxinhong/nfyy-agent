// Command gateway is the EAP Agent Gateway: the single public entry point.
// TASK-003 adds the identity slice: dev-mode OIDC login, employee API key
// lifecycle (hash-only storage, one-time display), /enterprise/v1/me,
// audience-scoped token exchange with server-validated workspace authority,
// JWKS publication, Redis-backed rate limiting and reliable auth audit events.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/capability"
	"enterprise-agent-platform/packages/catalog"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/dlp"
	"enterprise-agent-platform/packages/knowledge"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/policy"
	"enterprise-agent-platform/packages/skillshare"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/wiki"
)

// Config is the gateway runtime configuration, sourced from the environment.
type Config struct {
	Addr               string
	DatabaseURL        string
	RedisAddr          string
	MigrationsDir      string
	TokenIssuer        string
	RateLimitPerMinute int64
	DevMode            bool
	// LLM data plane (ADR-0006 glue). Empty ProviderURL keeps the
	// /v1/chat/completions route disabled.
	LLMProviderURL   string
	LLMProviderKey   string
	LLMModels        []string
	PolicyWorkspaces []string
	// Portal SPA hosting (ADR-0007): directory of the xingding-ui-cli
	// generated web build. Missing directory keeps static routes disabled.
	WebDist string
	// OIDC login boundary (TASK-009/013): standard OIDC tenant or the Feishu
	// profile. Credentials are supplied out of band via the environment and
	// never persisted in task evidence.
	OIDCIssuer        string
	OIDCProvider      string // "" (standard) | "feishu"
	OIDCClientID      string
	OIDCClientSecret  string
	OIDCRedirectURL   string
	OIDCDefaultGroups []string
	// CLI installer dist directory (cross-built eap binaries).
	CLIDist string
	// LocalAuth enables the built-in username/password login (TASK-027).
	LocalAuth bool
	// BlobRoot stores contribution attachments; empty disables uploads.
	BlobRoot string
	// ArtifactRoot holds the signed capability manifest and archives
	// (packages/catalog); empty keeps artifacts unpublished.
	ArtifactRoot string
}

func envCSV(key string) []string {
	var out []string
	for _, item := range strings.Split(os.Getenv(key), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "1" || v == "true" || v == "yes"
}

func loadConfig() Config {
	limit := int64(120)
	if v := os.Getenv("EAP_RATE_LIMIT_PER_MINUTE"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return Config{
		Addr:               envOr("EAP_GATEWAY_ADDR", ":8080"),
		DatabaseURL:        envOr("EAP_DATABASE_URL", "postgres://eap:eap@127.0.0.1:15432/eap?sslmode=disable"),
		RedisAddr:          envOr("EAP_REDIS_ADDR", "127.0.0.1:16379"),
		MigrationsDir:      envOr("EAP_MIGRATIONS_DIR", "migrations"),
		TokenIssuer:        envOr("EAP_TOKEN_ISSUER", "https://agent.company.com/"),
		RateLimitPerMinute: limit,
		DevMode:            envBool("EAP_DEV_MODE"),
		LLMProviderURL:     os.Getenv("EAP_LLM_PROVIDER_URL"),
		LLMProviderKey:     os.Getenv("EAP_LLM_PROVIDER_KEY"),
		LLMModels:          envCSV("EAP_LLM_MODELS"),
		PolicyWorkspaces:   envCSV("EAP_POLICY_WORKSPACES"),
		WebDist:            envOr("EAP_WEB_DIST", "apps/web/dist"),
		OIDCIssuer:         os.Getenv("EAP_OIDC_ISSUER"),
		OIDCProvider:       os.Getenv("EAP_OIDC_PROVIDER"),
		OIDCClientID:       os.Getenv("EAP_OIDC_CLIENT_ID"),
		OIDCClientSecret:   os.Getenv("EAP_OIDC_CLIENT_SECRET"),
		OIDCRedirectURL:    envOr("EAP_OIDC_REDIRECT_URL", "http://127.0.0.1:8080/enterprise/v1/auth/oidc/callback"),
		OIDCDefaultGroups:  envCSV("EAP_OIDC_DEFAULT_GROUPS"),
		CLIDist:            envOr("EAP_CLI_DIST_DIR", "dist/cli"),
		LocalAuth:          envBool("EAP_LOCAL_AUTH"),
		ArtifactRoot:       os.Getenv("EAP_ARTIFACT_ROOT"),
		BlobRoot:           os.Getenv("EAP_BLOB_ROOT"),
	}
}

func main() {
	if err := run(loadConfig()); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}

func run(cfg Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// TASK-040: explicit pool sizing instead of the pgx default
	// max(4, NumCPU). The compaction work (TASK-036..038) puts the steady-
	// state request at ~6-8 serial round trips; 32 connections sustain the
	// concurrency load gate (TASK-041) with headroom for control-plane and
	// batch writers without pool queueing.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("pool config: %w", err)
	}
	poolCfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("postgres pool: %w", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, cfg.MigrationsDir); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()

	st := store.New(pool)
	contentStore := content.New(pool, st)
	directory, err := catalog.New(cfg.ArtifactRoot)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	signingKey, err := st.EnsureSigningKey(ctx, store.GenerateSigningKeyDER)
	if err != nil {
		return fmt.Errorf("signing key: %w", err)
	}
	publicKeys, err := st.ListSigningPublicKeys(ctx)
	if err != nil {
		return fmt.Errorf("signing public keys: %w", err)
	}
	verificationKeys := toVerificationKeys(publicKeys)
	verifier, err := token.NewEd25519Verifier(verificationKeys...)
	if err != nil {
		return fmt.Errorf("token verifier: %w", err)
	}
	signer, err := token.NewEd25519Signer(signingKey.Kid, cfg.TokenIssuer, signingKey.PrivateKey)
	if err != nil {
		return fmt.Errorf("token signer: %w", err)
	}
	jwks, err := token.BuildJWKS(verificationKeys...)
	if err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	jwksBody, err := json.Marshal(jwks)
	if err != nil {
		return fmt.Errorf("jwks marshal: %w", err)
	}

	// TASK-010: production Policy/DLP engines load versioned rules from the
	// control tables; the LLM router resolves its plan from the same store.
	// Knowledge Hub (TASK-012): slice over the same pool with the V1
	// deterministic embedder; provider-backed embeddings land behind the
	// same Embedder seam (follow-up).
	srvKnowledge := knowledge.NewService(pool, knowledge.HashEmbedder{})

	// TASK-036 (ADR-0025): read-mostly control data behind the request path
	// is cached with a 60s staleness bound; per-request authorization facts
	// (key validity, membership) stay live.
	caches := NewControlCaches(ControlCacheTTL, ControlSourcesFromStore(st))
	tablePolicy := &policy.TablePolicy{LoadRules: caches.PolicyRules()}
	tableDLP := dlp.NewTableScanner(st.LoadDLPRules)
	// TASK-028/037: tokenize-class rules mint workspace-scoped pseudonyms
	// backed by the dlp_tokens vault; the workspace rides on the request
	// context. The batch seam resolves a whole scan in one vault exchange.
	tableDLP.Tokenizer = func(ctx context.Context, ruleName, value string) (string, error) {
		ws, _ := ctx.Value(dlpWorkspaceKey{}).(string)
		return st.TokenizeDLP(ctx, ws, ruleName, value)
	}
	tableDLP.TokenizeBatch = func(ctx context.Context, ruleName string, values []string) (map[string]string, error) {
		ws, _ := ctx.Value(dlpWorkspaceKey{}).(string)
		return st.BatchTokenizeDLP(ctx, ws, ruleName, values)
	}
	providerHTTP := llmkernel.NewProviderHTTPClient()
	routedLLM := &llmkernel.Router{
		ResolvePlan:   caches.ResolvePlan(),
		ResolveTarget: caches.ResolveTarget(),
		Credential: func(ctx context.Context, ref string) (string, error) {
			return mcpgateway.EnvResolver{}.Resolve(ctx, ref)
		},
		Client: &llmkernel.OpenAICompatClient{Client: providerHTTP},
	}

	srv := &Server{
		LocalAuth: cfg.LocalAuth,
		PG:        PgPinger{Pool: pool},
		Redis:     RedisPinger{Client: rdb},
		Store:     st,
		Signer:    signer,
		Verifier:  verifier,
		JWKSBody:  jwksBody,
		// TASK-044: local fixed window — zero request-path round trips.
		// Multi-instance semantics: per-instance limit, aggregate = Limit x N.
		Rate:          &LocalWindow{Limit: cfg.RateLimitPerMinute, Window: time.Minute},
		DevMode:       cfg.DevMode,
		OIDCStore:     st,
		Members:       st,
		Skills:        skillshare.New(pool, st),
		Content:       contentStore,
		Curation:      curation.New(pool, st, contentStore),
		Contributions: contribution.New(pool, st),
		Wiki:          wiki.NewStore(pool, st),
		Catalog:       directory,
		Policy:        tablePolicy,
		DLP:           tableDLP,
		LLMRouter:     routedLLM,
		Pricing:       caches.PricingKnown(),
		BatchReveal: func(ctx context.Context, workspaceID string, tokens []string) (map[string]store.DLPReveal, error) {
			return st.BatchRevealDLP(ctx, workspaceID, tokens)
		},
		AuthChain:   st.GetAPIKeyAuthByHash,
		AuditWriter: st,
		Knowledge:   srvKnowledge,
		MCP: &mcpgateway.Service{
			Registry: st,
			Resolver: mcpgateway.EnvResolver{},
			Executor: &mcpgateway.HTTPExecutor{Client: providerHTTP},
			Audit:    st,
		},
	}
	// TASK-040/041: bounded background applier for api_keys last-used
	// touches; lives for the process lifetime.
	srv.StartTouchApplier(ctx)
	// Capability Gateway (TASK-034): one meta-tool surface over the governed
	// MCP registry and released skills. Dual-surface: /mcp/{server} stays.
	mcpListAll := func(ctx context.Context, workspaceID string) ([]mcpgateway.RegisteredTool, error) {
		rows, err := pool.Query(ctx, `SELECT DISTINCT server FROM mcp_tools ORDER BY server`)
		if err != nil {
			return nil, err
		}
		var servers []string
		for rows.Next() {
			var srv string
			if err := rows.Scan(&srv); err != nil {
				rows.Close()
				return nil, err
			}
			servers = append(servers, srv)
		}
		rows.Close()
		var out []mcpgateway.RegisteredTool
		for _, srv := range servers {
			tools, err := st.ListToolsForWorkspace(ctx, srv, workspaceID)
			if err != nil {
				return nil, err
			}
			out = append(out, tools...)
		}
		return out, nil
	}
	mcpSvc := srv.MCP
	capRegistry := &capability.Registry{
		Sources: []capability.Source{
			&capability.MCPSource{Service: mcpSvc, Policy: mcpSvc.Policy, ListTools: mcpListAll},
			&capability.SkillSource{
				ListSkills: func(ctx context.Context, workspaceID string) ([]capability.SkillEntry, error) {
					rows, err := st.ListReleasedSkillsForWorkspace(ctx, workspaceID)
					if err != nil {
						return nil, err
					}
					entries := make([]capability.SkillEntry, 0, len(rows))
					for _, r := range rows {
						entries = append(entries, capability.SkillEntry{Name: r.Name, Version: r.Version, Description: "released skill package"})
					}
					return entries, nil
				},
				ResolveSkill: func(ctx context.Context, workspaceID, name, version string) (capability.SkillEntry, error) {
					e, err := st.ResolveReleasedSkill(ctx, workspaceID, name, version)
					if err != nil {
						return capability.SkillEntry{}, err
					}
					return capability.SkillEntry{Name: e.Name, Version: e.Version, Description: "released skill package"}, nil
				},
			},
		},
	}
	srv.Capabilities = capRegistry

	// OIDC login boundary: standard OIDC tenant or the Feishu profile. The
	// client secret stays in the server environment; nothing identity-related
	// reaches the browser beyond the opaque session cookie.
	switch {
	case cfg.OIDCProvider == "feishu" && cfg.OIDCClientID != "" && cfg.OIDCClientSecret != "":
		srv.OIDCFeishu = &oidc.FeishuConfig{
			ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL, DefaultGroups: cfg.OIDCDefaultGroups,
		}
		log.Printf("gateway: oidc login enabled (profile=feishu, client=%s)", cfg.OIDCClientID)
	case cfg.OIDCIssuer != "" && cfg.OIDCClientID != "" && cfg.OIDCClientSecret != "":
		srv.OIDC = &oidc.Config{
			Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
			RedirectURL: cfg.OIDCRedirectURL, Scopes: []string{"openid", "email", "profile", "groups"},
		}
		log.Printf("gateway: oidc login enabled (profile=standard, issuer=%s)", cfg.OIDCIssuer)
	}

	// Portal SPA hosting: enabled when the CLI-generated web build exists.
	// (apps/web is generated by xingding-ui-cli and built with pnpm; see
	// ADR-0003/ADR-0007 — the gateway only hosts the output, never hand-
	// written pages.)
	if cfg.BlobRoot != "" {
		srv.Blobs = blob.New(pool, st, cfg.BlobRoot)
		if addr := os.Getenv("EAP_CLAMD_ADDR"); addr != "" {
			srv.Blobs.WithScanner(blob.Clamd{Addr: addr})
			log.Printf("gateway: attachments scanned by clamd at %s", addr)
		} else {
			log.Printf("gateway: no malware scanner (EAP_CLAMD_ADDR); attachments stay unscanned")
		}
	}
	if path := os.Getenv("EAP_PACKAGE_KEY_FILE"); path != "" {
		keys, err := loadPackageKey(ctx, path, st)
		if err != nil {
			return fmt.Errorf("package key: %w", err)
		}
		srv.PackageKeys = keys
		log.Printf("gateway: skill releases signed with the key in %s", path)
	} else {
		log.Printf("gateway: no EAP_PACKAGE_KEY_FILE; skill versions cannot be released")
	}
	if srv.LocalAuth {
		adminPassword := envOr("EAP_LOCAL_ADMIN_PASSWORD", "eap@2026")
		if err := srv.ensureDefaultLocalAdmin(ctx, adminPassword); err != nil {
			log.Printf("gateway: default local admin not provisioned: %v", err)
		}
	}
	if _, statErr := os.Stat(cfg.CLIDist); statErr == nil {
		srv.CLIDist = cfg.CLIDist
		log.Printf("gateway: cli installer served from %s", cfg.CLIDist)
	}
	if _, statErr := os.Stat(cfg.WebDist); statErr == nil {
		site, werr := NewStaticSite(cfg.WebDist)
		if werr != nil {
			return fmt.Errorf("web dist: %w", werr)
		}
		srv.Web = site
		log.Printf("gateway: portal ui served from %s", cfg.WebDist)
	} else {
		log.Printf("gateway: portal ui disabled (no dist at %s)", cfg.WebDist)
	}

	// LLM data plane: the routed path above is the production surface. The
	// env-based single-provider kernel remains only as a documented dev
	// fallback when no provider control data exists.
	if cfg.LLMProviderURL != "" && len(cfg.LLMModels) > 0 {
		srv.Usage = logUsageRecorder{}
		log.Printf("gateway: llm env fallback retained (provider %s, models %v)", cfg.LLMProviderURL, cfg.LLMModels)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	log.Printf("gateway: version %s listening on %s (dev_mode=%t)", Version, cfg.Addr, cfg.DevMode)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		log.Printf("gateway: stopped")
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server: %w", err)
	}
}

// firstOr returns the first item or the fallback when the list is empty.
func firstOr(items []string, fallback string) string {
	if len(items) > 0 {
		return items[0]
	}
	return fallback
}

// logUsageRecorder is the V0 usage sink: one structured log line per call.
// The usage_records table arrives with the M2 cost/quota milestone.
type logUsageRecorder struct{}

func (logUsageRecorder) Record(_ context.Context, r llmkernel.UsageRecord) {
	b, _ := json.Marshal(r)
	log.Printf("gateway: usage %s", b)
}

func toVerificationKeys(keys []store.SigningKey) []token.VerificationKey {
	out := make([]token.VerificationKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, token.VerificationKey{KeyID: k.Kid, Public: k.PublicKey})
	}
	return out
}
