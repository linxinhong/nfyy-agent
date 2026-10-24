package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"crypto/ed25519"
	"enterprise-agent-platform/packages/audit"
	"enterprise-agent-platform/packages/blob"
	"enterprise-agent-platform/packages/capability"
	"enterprise-agent-platform/packages/catalog"
	"enterprise-agent-platform/packages/content"
	"enterprise-agent-platform/packages/contribution"
	"enterprise-agent-platform/packages/curation"
	"enterprise-agent-platform/packages/errs"

	"enterprise-agent-platform/packages/governance"
	"enterprise-agent-platform/packages/identity"
	"enterprise-agent-platform/packages/knowledge"
	"enterprise-agent-platform/packages/llmkernel"
	"enterprise-agent-platform/packages/mcpgateway"
	"enterprise-agent-platform/packages/oidc"
	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillshare"
	"enterprise-agent-platform/packages/store"
	"enterprise-agent-platform/packages/token"
	"enterprise-agent-platform/packages/tracing"
	"enterprise-agent-platform/packages/wiki"
)

// Version is the gateway build version.
const Version = "0.1.0"

// Pinger is the readiness probe contract implemented by storage clients.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PgPinger adapts pgxpool to Pinger.
type PgPinger struct{ Pool *pgxpool.Pool }

func (p PgPinger) Ping(ctx context.Context) error { return p.Pool.Ping(ctx) }

// RedisPinger adapts go-redis to Pinger.
type RedisPinger struct{ Client *redis.Client }

func (p RedisPinger) Ping(ctx context.Context) error { return p.Client.Ping(ctx).Err() }

// ControlStore is the persistence surface the gateway needs. *store.Store is
// the production implementation; tests inject fakes. Embedding audit.Writer
// keeps the reliable audit path reachable from the auth middleware.
type ControlStore interface {
	audit.Writer
	UpsertUser(ctx context.Context, claims store.IDClaims) (store.User, error)
	GetUser(ctx context.Context, id string) (store.User, error)
	IsMember(ctx context.Context, userID, workspaceID string) (identity.Membership, error)
	ListMemberships(ctx context.Context, userID string) ([]store.Membership, error)
	SyncMemberships(ctx context.Context, userID string, workspaceIDs []string, roles []string) error
	ListDLPRulesAll(ctx context.Context) ([]store.AdminDLPRule, error)
	UpdateDLPRuleAction(ctx context.Context, name, action string, enabled bool, exceptRoles []string) (store.AdminDLPRule, error)
	GetLocalCredential(ctx context.Context, username string) (string, string, error)
	TokenizeDLP(ctx context.Context, workspaceID, ruleName, value string) (string, error)
	RevealDLP(ctx context.Context, workspaceID, token string) (string, string, error)
	UpsertLocalCredential(ctx context.Context, username, userID, passwordHash string) error
	LocalPasswordChangeRequired(ctx context.Context, username string) (bool, error)
	RequireLocalPasswordChange(ctx context.Context, username string) error
	ResolveGroupBindings(ctx context.Context, groups []string) ([]store.GroupBinding, error)
	SyncGroupBindings(ctx context.Context, userID string, bindings []store.GroupBinding) error
	CreateAPIKey(ctx context.Context, userID, keyHash, prefix, name, workspaceID string, expiresAt *time.Time, created audit.Event) (store.APIKey, error)
	GetAPIKeyByHash(ctx context.Context, keyHash string) (store.APIKey, error)
	ListAPIKeys(ctx context.Context, userID string) ([]store.APIKey, error)
	RevokeAPIKey(ctx context.Context, userID, keyID string, revoked audit.Event) (bool, error)
	TouchAPIKey(ctx context.Context, keyID, lastIP, clientID string)
	RoutePlanFor(ctx context.Context, alias string) (llmkernel.RoutePlanView, error)
	ListEnabledAliases(ctx context.Context) ([]string, error)
	ListPackageKeys(ctx context.Context) ([]skillreg.PkgKey, error)
	ActivePackageKeyID(ctx context.Context) (string, error)
	ResolveProviderTarget(ctx context.Context, provider string) (llmkernel.ProviderTarget, error)
	PricingKnown(ctx context.Context, alias, provider string) (governance.Pricing, bool, int, error)
	InsertUsage(ctx context.Context, u governance.UsageRecord) error
}

// Server carries the gateway dependencies and exposes the HTTP surface.
// Handlers are plain methods so tests can inject stubs. Store == nil keeps the
// TASK-001 health-only skeleton (no enterprise routes registered).
type Server struct {
	PG        Pinger
	Redis     Pinger
	Store     ControlStore
	Signer    token.Signer
	Verifier  token.Verifier
	JWKSBody  []byte
	Rate      RateLimiter
	DevMode   bool
	Now       func() time.Time
	MCP       *mcpgateway.Service
	LLM       *llmkernel.Kernel
	LLMRouter *llmkernel.Router
	Policy    llmkernel.PolicyEngine
	DLP       llmkernel.DLPEngine
	// Pricing resolves usage-ledger pricing for (alias, provider). Wired to
	// the control cache in production (ADR-0025); nil falls back to the
	// store read on every request.
	Pricing func(ctx context.Context, alias, provider string) (governance.Pricing, bool, int, error)
	// BatchReveal restores many DLP pseudonyms in one vault read (TASK-037).
	// Wired to the store batch method in production; nil falls back to the
	// per-token RevealDLP loop. Fail-closed semantics are identical.
	BatchReveal func(ctx context.Context, workspaceID string, tokens []string) (map[string]store.DLPReveal, error)
	// AuthChain resolves a presented API key to (key, owner) plus the live
	// memberships of both candidate workspaces in one round trip (TASK-038,
	// fused with membership in TASK-043). Revocation/expiry/membership are
	// still judged per request from live columns; nil falls back to the
	// two-step key + user reads and the per-request membership resolver.
	AuthChain func(ctx context.Context, keyHash, headerWorkspace string) (store.APIKeyAuth, error)
	// AuditWriter is the reliable audit sink (TASK-038). Wired to the store
	// in production and tests that need batch counting; nil falls back to
	// Store (which is an audit.Writer).
	AuditWriter audit.Writer
	// Credential resolves provider credential references (env in dev;
	// Vault/KMS-shaped resolvers later). Shared by MCP and LLM data planes.
	Credential func(ctx context.Context, ref string) (string, error)
	// touchQueue bounds the async API-key last-used applier (TASK-041 gate
	// finding): drops instead of piling up unbounded goroutines when the
	// database is saturated. Best-effort semantics unchanged.
	touchQueue chan touchReq
	// OIDC enables the production Portal login boundary when non-nil.
	OIDC *oidc.Config
	// OIDCFeishu selects the Feishu profile instead of standard OIDC when
	// set; the two are mutually exclusive and the standard profile wins.
	OIDCFeishu *oidc.FeishuConfig
	// CLIDist is the directory holding cross-built eap binaries (dist/cli).
	// Missing directory keeps the public installer routes disabled.
	CLIDist string
	// LocalAuth enables the built-in username/password login (TASK-027).
	LocalAuth bool
	// Capabilities is the capability gateway registry (TASK-034); nil keeps
	// /mcp/agent unregistered.
	Capabilities *capability.Registry
	JWKSFetch    func(ctx context.Context, issuer string) (oidc.JSONKeySet, error)
	// Catalog is the skill/capability directory; nil keeps the catalog and
	// capability-package routes unregistered.
	Catalog *catalog.Directory
	// Wiki is the shelf/book/chapter node store; with Content it enables the
	// /enterprise/v1/wiki routes.
	Wiki *wiki.Store
	// Curation holds curation proposals; with Content it enables the
	// /enterprise/v1/curation routes.
	Curation *curation.Store
	// Blobs stores contribution attachments; nil (no EAP_BLOB_ROOT) keeps the
	// upload route unregistered and every cited attachment missing.
	Blobs *blob.Store
	// Contributions is the upload inbox; it needs Content for member groups,
	// so both must be set for the contribution routes to register.
	Contributions *contribution.Store
	// Content is the shared content store; nil keeps /enterprise/v1/resources
	// and the review queue unregistered.
	Content *content.Store
	// Members is the Workspace member/group administration store; nil keeps
	// the member administration routes unregistered.
	Members MemberStore
	// Skills scopes the Skill Registry for MindLink sharing (personal, team,
	// organization; protected SKILL.md). With Content it registers the
	// member skill routes; nil keeps the registry Workspace-wide.
	Skills *skillshare.Store
	// OIDCStore is the concrete store for session/flow tables.
	OIDCStore  *store.Store
	OIDCSecure bool
	// Pool exposes raw pgx access where a handle is already available.
	Pool      *pgxpool.Pool
	Usage     llmkernel.UsageRecorder
	Web       *StaticSite
	Knowledge *knowledge.Service
	// PackageKeys holds pkg- namespace private signing keys by kid (loaded
	// from the key-management boundary; never serialized).
	PackageKeys map[string]ed25519.PrivateKey
}

// Handler builds the full middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /debug/request", s.handleDebugRequest)
	if s.Store != nil {
		mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
		mux.HandleFunc("POST /enterprise/v1/auth/dev-login", s.handleDevLogin)
		mux.HandleFunc("POST /enterprise/v1/keys", s.handleCreateKey)
		mux.HandleFunc("GET /enterprise/v1/keys", s.handleListKeys)
		mux.HandleFunc("DELETE /enterprise/v1/keys/{keyID}", s.handleRevokeKey)
		mux.HandleFunc("GET /enterprise/v1/me", s.handleMe)
		mux.HandleFunc("GET /enterprise/v1/profile", s.handleProfile)
		mux.HandleFunc("POST /enterprise/v1/token/exchange", s.handleExchange)
		if s.MCP != nil {
			mux.HandleFunc("POST /mcp/{server}", s.handleMCP)
		}
		if s.LLMRouter != nil || s.LLM != nil {
			mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
			mux.HandleFunc("GET /v1/models", s.handleModels)
			mux.HandleFunc("POST /v1/responses", s.handleResponses)
			mux.HandleFunc("POST /v1/embeddings", s.handleEmbeddings)
			mux.HandleFunc("POST /v1/messages", s.handleMessages)
		}
		if s.Store != nil {
			mux.HandleFunc("GET /enterprise/v1/skills", s.handleSkillCatalog)
			mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions", s.handleSkillPublish)
			mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/review", s.handleSkillReview)
			mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/release", s.handleSkillRelease)
			mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/deprecate", s.handleSkillDeprecate)
			mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/revoke", s.handleSkillRevoke)
			mux.HandleFunc("GET /enterprise/v1/skills/{name}/versions/{version}/download", s.handleSkillDownload)
			mux.HandleFunc("GET /.well-known/package-keys.json", s.handlePackageKeys)
			if s.Skills != nil && s.Content != nil {
				mux.HandleFunc("GET /enterprise/v1/me/skills", s.handleMySkills)
				mux.HandleFunc("PUT /enterprise/v1/me/skills/{name}", s.handleEnableSkill)
				mux.HandleFunc("DELETE /enterprise/v1/me/skills/{name}", s.handleEnableSkill)
				mux.HandleFunc("PUT /enterprise/v1/skills/{name}/requirement", s.handleRequireSkill)
				mux.HandleFunc("DELETE /enterprise/v1/skills/{name}/requirement", s.handleRequireSkill)
				mux.HandleFunc("GET /enterprise/v1/skill-reviews", s.handleSkillReviewQueue)
				mux.HandleFunc("GET /enterprise/v1/skills/{name}/versions/{version}/content", s.handleSkillContent)
				mux.HandleFunc("POST /enterprise/v1/skills/{name}/versions/{version}/reject", s.handleSkillReject)
			}
		}
		if s.Knowledge != nil {
			mux.HandleFunc("POST /enterprise/v1/knowledge/documents", s.handleKnowledgeUpload)
			mux.HandleFunc("POST /enterprise/v1/knowledge/search", s.handleKnowledgeSearch)
		}
		mux.HandleFunc("POST /enterprise/v1/auth/device/code", s.handleDeviceCode)
		mux.HandleFunc("POST /enterprise/v1/auth/device/approve", s.handleDeviceApprove)
		mux.HandleFunc("POST /enterprise/v1/auth/device/deny", s.handleDeviceDeny)
		mux.HandleFunc("POST /enterprise/v1/auth/device/token", s.handleDeviceToken)
		mux.HandleFunc("POST /enterprise/v1/auth/device/logout", s.handleDeviceLogout)
		mux.HandleFunc("GET /enterprise/v1/cli/bootstrap", s.handleCLIBootstrap)
		mux.HandleFunc("GET /enterprise/v1/agents/{client}", s.handleAgentList)
		mux.HandleFunc("POST /enterprise/v1/agents/{client}/ticket", s.handleAgentTicket)
		mux.HandleFunc("POST /enterprise/v1/agents/{client}/download", s.handleAgentDownload)
		// TASK-022: governed read-only admin inspection surface.
		mux.HandleFunc("GET /enterprise/v1/admin/users", s.handleAdminUsers)
		mux.HandleFunc("GET /enterprise/v1/admin/agents", s.handleAdminAgents)
		mux.HandleFunc("GET /enterprise/v1/admin/models", s.handleAdminModels)
		mux.HandleFunc("GET /enterprise/v1/admin/usage", s.handleAdminUsage)
		mux.HandleFunc("GET /enterprise/v1/admin/mcp", s.handleAdminMCP)
		mux.HandleFunc("GET /enterprise/v1/admin/knowledge", s.handleAdminKnowledge)
		mux.HandleFunc("GET /enterprise/v1/admin/dlp", s.handleAdminDLP)
		mux.HandleFunc("GET /enterprise/v1/admin/audit", s.handleAdminAudit)
		if s.Catalog != nil {
			mux.HandleFunc("GET /enterprise/v1/catalog/search", s.handleCatalogSearch)
			mux.HandleFunc("GET /enterprise/v1/catalog/items/{id}", s.handleCatalogItem)
			mux.HandleFunc("GET /enterprise/v1/capability-packages/{id}/manifest", s.handleCapabilityManifest)
			mux.HandleFunc("GET /enterprise/v1/capability-packages/{id}/artifacts/{filename}", s.handleCapabilityArtifact)
		}
		if s.Content != nil {
			mux.HandleFunc("GET /enterprise/v1/resources", s.handleListResources)
			mux.HandleFunc("POST /enterprise/v1/resources", s.handleCreateResource)
			mux.HandleFunc("GET /enterprise/v1/resources/{id}", s.handleGetResource)
			mux.HandleFunc("PUT /enterprise/v1/resources/{id}", s.handleEditResource)
			mux.HandleFunc("POST /enterprise/v1/resources/{id}/submit", s.handleResourceTransition("submit"))
			mux.HandleFunc("POST /enterprise/v1/resources/{id}/approve", s.handleResourceTransition("approve"))
			mux.HandleFunc("POST /enterprise/v1/resources/{id}/reject", s.handleResourceTransition("reject"))
			mux.HandleFunc("POST /enterprise/v1/resources/{id}/withdraw", s.handleResourceTransition("withdraw"))
			mux.HandleFunc("GET /enterprise/v1/review/queue", s.handleReviewQueue)
			if s.Curation != nil {
				mux.HandleFunc("GET /enterprise/v1/curation/proposals", s.handleListProposals)
				mux.HandleFunc("POST /enterprise/v1/curation/proposals/{id}/decision", s.handleDecideProposal)
				mux.HandleFunc("POST /enterprise/v1/curation/sweeps", s.handleRequestSweep)
			}
			if s.Wiki != nil {
				mux.HandleFunc("GET /enterprise/v1/wiki/tree", s.handleWikiTree)
				mux.HandleFunc("GET /enterprise/v1/wiki/search", s.handleWikiSearch)
				mux.HandleFunc("GET /enterprise/v1/wiki/pages/{id}", s.handleWikiPage)
				mux.HandleFunc("GET /enterprise/v1/wiki/by-path/{path...}", s.handleWikiPageByPath)
				mux.HandleFunc("POST /enterprise/v1/wiki/nodes", s.handleCreateWikiNode)
				mux.HandleFunc("PATCH /enterprise/v1/wiki/nodes/{id}", s.handleUpdateWikiNode)
				mux.HandleFunc("DELETE /enterprise/v1/wiki/nodes/{id}", s.handleDeleteWikiNode)
				mux.HandleFunc("POST /mcp/wiki", s.handleWikiMCP)
				if s.Blobs != nil {
					mux.HandleFunc("GET /enterprise/v1/wiki/pages/{id}/attachments/{sha256}", s.handleWikiAttachment)
				}
			}
			if s.Contributions != nil {
				mux.HandleFunc("POST /enterprise/v1/contributions", s.handleUploadContribution)
				mux.HandleFunc("GET /enterprise/v1/contributions", s.handleListContributions)
				mux.HandleFunc("GET /enterprise/v1/contributions/{id}", s.handleGetContribution)
				mux.HandleFunc("POST /enterprise/v1/contributions/{id}/withdraw", s.handleWithdrawContribution)
				if s.Blobs != nil {
					mux.HandleFunc("PUT /enterprise/v1/blobs/{sha256}", s.handlePutBlob)
				}
			}
		}
		if s.Members != nil {
			mux.HandleFunc("GET /enterprise/v1/admin/members", s.handleAdminListMembers)
			mux.HandleFunc("PATCH /enterprise/v1/admin/members/{userID}", s.handleAdminUpdateMember)
			mux.HandleFunc("DELETE /enterprise/v1/admin/members/{userID}", s.handleAdminRemoveMember)
			mux.HandleFunc("GET /enterprise/v1/admin/groups", s.handleAdminListGroups)
			mux.HandleFunc("POST /enterprise/v1/admin/groups", s.handleAdminCreateGroup)
			mux.HandleFunc("PATCH /enterprise/v1/admin/groups/{groupID}", s.handleAdminRenameGroup)
			mux.HandleFunc("DELETE /enterprise/v1/admin/groups/{groupID}", s.handleAdminDeleteGroup)
			mux.HandleFunc("GET /enterprise/v1/admin/roles", s.handleAdminListRoles)
			mux.HandleFunc("PUT /enterprise/v1/admin/roles/{key}", s.handleAdminPutRole)
			mux.HandleFunc("DELETE /enterprise/v1/admin/roles/{key}", s.handleAdminDeleteRole)
			mux.HandleFunc("GET /enterprise/v1/me/permissions", s.handleMyPermissions)
		}
		if s.OIDC != nil || s.OIDCFeishu != nil {
			mux.HandleFunc("GET /enterprise/v1/auth/oidc/login", s.startOIDCLogin)
			mux.HandleFunc("GET /enterprise/v1/auth/oidc/callback", s.oidcCallback)
			mux.HandleFunc("POST /enterprise/v1/auth/oidc/logout", s.logoutOIDC)
			mux.HandleFunc("GET /enterprise/v1/auth/oidc/profile", s.handleOIDCProfile)
		}
		if s.Web != nil {
			// Root lands on the Portal (the layout guard bootstraps the OIDC
			// cookie session or redirects to login).
			mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/portal/overview", http.StatusFound)
			})
			// Catch-all: ServeMux gives precedence to the registered API
			// patterns above; this serves the Portal SPA or JSON-404s APIs.
			mux.Handle("/", s.Web.Handler())
		}
	}
	// CLI installer routes are public and independent of the control store.
	if s.CLIDist != "" {
		mux.HandleFunc("GET /cli/install.sh", s.handleCLIInstallScript)
		mux.HandleFunc("GET /cli/install/sha256sums", s.handleCLIChecksums)
		mux.HandleFunc("GET /cli/install/manifest.json", s.handleCLIManifest)
		mux.HandleFunc("GET /cli/install/{platform}", s.handleCLIInstallBinary)
	}
	// TASK-026: governed DLP rule management (list + action/enabled toggle).
	mux.HandleFunc("GET /enterprise/v1/admin/dlp/rules", s.handleAdminDLPRules)
	mux.HandleFunc("POST /enterprise/v1/admin/dlp/test", s.handleAdminDLPRuleTest)
	mux.HandleFunc("PATCH /enterprise/v1/admin/dlp/rules/{name}", s.handleAdminUpdateDLPRule)
	// TASK-027: built-in local account/password login (operator-gated).
	if s.LocalAuth {
		mux.HandleFunc("POST /enterprise/v1/auth/local/login", s.handleLocalLogin)
		mux.HandleFunc("POST /enterprise/v1/admin/local-accounts", s.handleAdminCreateLocalAccount)
		if s.Members != nil {
			mux.HandleFunc("POST /enterprise/v1/admin/members", s.handleAdminCreateMember)
			mux.HandleFunc("POST /enterprise/v1/auth/local/password", s.handleLocalPasswordChange)
		}
	}
	mux.HandleFunc("POST /enterprise/v1/dlp/reveal", s.handleDLPReveal)
	if s.Capabilities != nil {
		mux.HandleFunc("POST /mcp/agent", s.handleAgentMCP)
	}
	// Public capability probe: which login methods this gateway exposes.
	mux.HandleFunc("GET /enterprise/v1/auth/methods", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"local": s.LocalAuth, "dev": s.DevMode})
	})
	return s.common(mux)
}

// common wraps every handler with trace propagation and panic recovery.
// A recovered panic becomes a frozen-shape internal problem, never a stack.
func (s *Server) common(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := tracing.EnsureHeaders(r.Header)
		echoTraceHeaders(w, r)
		r = r.WithContext(tracing.WithRequest(r.Context(), meta))
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("gateway: panic on %s %s: %v", r.Method, r.URL.Path, rec)
				writeProblem(w, r, errs.New(errs.Internal, "internal error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func echoTraceHeaders(w http.ResponseWriter, r *http.Request) {
	for _, k := range []string{tracing.HeaderTraceparent, tracing.HeaderRequestID} {
		w.Header().Set(k, r.Header.Get(k))
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

// handleReadyz reports dependency readiness. Failing dependencies yield 503
// with the failing check names; details never include credentials.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks := map[string]string{
		"postgres": probe(ctx, s.PG),
		"redis":    probe(ctx, s.Redis),
	}
	status := "ready"
	code := http.StatusOK
	for _, res := range checks {
		if res != "ok" {
			status = "not_ready"
			code = http.StatusServiceUnavailable
			break
		}
	}
	writeJSON(w, code, map[string]any{
		"status":  status,
		"version": Version,
		"checks":  checks,
	})
}

func probe(ctx context.Context, p Pinger) string {
	if err := p.Ping(ctx); err != nil {
		return "error"
	}
	return "ok"
}

// handleDebugRequest demonstrates the trace propagation contract: inbound
// traceparents are kept, missing ones are minted, and the canonical headers
// are always echoed back.
func (s *Server) handleDebugRequest(w http.ResponseWriter, r *http.Request) {
	meta := tracing.EnsureHeaders(r.Header)
	echoTraceHeaders(w, r)
	writeJSON(w, http.StatusOK, map[string]any{
		"trace_id":      meta.Trace.TraceID,
		"request_id":    meta.RequestID,
		"inbound_trace": meta.InboundTrace,
		"traceparent":   meta.Trace.Traceparent(),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("gateway: encode response: %v", err)
	}
}

// writeProblem serializes failures in the frozen error contract.
func writeProblem(w http.ResponseWriter, r *http.Request, err error) {
	problem, ok := errs.From(err)
	if !ok {
		problem = errs.Problem{Code: errs.Internal, Message: "internal error"}
	}
	if meta, ok := tracing.FromContext(r.Context()); ok {
		problem.TraceID = meta.Trace.TraceID
	}
	writeJSON(w, problem.Code.HTTPStatus(), problem)
}
