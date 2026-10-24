// Package contribution is the inbox layer for knowledge uploaded by local
// agents: one contribution per (author, local page), append-only versions,
// structured rejection reasons the agent can act on without a human.
//
// Why: uploads are written by agents from a local llm-wiki style store, so a
// rejection must say which field failed and why (Reason codes are part of the
// public contract), and repeating an upload must be harmless.
//
// Invariants and pitfalls:
//   - Reason codes and the never-upload detail keys are contract surface
//     (api/mindlink-openapi.yaml); renaming one breaks deployed agents.
//   - ContentHash covers every field that changes meaning; the request
//     idempotency hash additionally covers the scope, mode and client, so a
//     reused Idempotency-Key with any different byte is refused.
//   - Validation reports every problem at once; agents fix all in one pass.
package contribution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SchemaVersion is the upload schema this server accepts.
const SchemaVersion = 1

const (
	ScopePersonal     = "personal"
	ScopeTeam         = "team"
	ScopeOrganization = "organization"
)

// Reason codes (contract surface).
const (
	ReasonRequired          = "required"
	ReasonTooLong           = "too_long"
	ReasonTooMany           = "too_many"
	ReasonInvalidFormat     = "invalid_format"
	ReasonNotRedacted       = "not_redacted"
	ReasonForbiddenField    = "forbidden_field"
	ReasonSensitiveContent  = "sensitive_content"
	ReasonUnknownGroup      = "unknown_group"
	ReasonScopeNotAllowed   = "scope_not_allowed"
	ReasonAttachmentMissing = "attachment_missing"
	ReasonSchemaUnsupported = "schema_unsupported"
)

const (
	maxLocalIDBytes   = 128
	maxTitleRunes     = 160
	maxSummaryRunes   = 300
	maxMarkdownBytes  = 200_000
	maxDetailsBytes   = 16_384
	maxLinks          = 200
	maxSourceRefs     = 100
	maxRefBytes       = 2_048
	maxAttachments    = 20
	maxAttachmentSize = 50 << 20
	maxClientField    = 64
)

// Reason is one actionable rejection cause.
type Reason struct {
	Field   string   `json:"field"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Rules   []string `json:"rules,omitempty"` // DLP rule ids for sensitive_content
}

// Link points at another local page of the same author.
type Link struct {
	TargetLocalID string `json:"target_local_id"`
	Label         string `json:"label,omitempty"`
}

// SourceRef is metadata about a source; the source itself is never uploaded.
type SourceRef struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Title string `json:"title,omitempty"`
}

// Attachment references a blob uploaded beforehand by content hash.
type Attachment struct {
	SHA256    string `json:"sha256"`
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
}

// Client identifies the uploading agent build.
type Client struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	SchemaVersion int    `json:"schema_version"`
}

// Upload is one snapshot of a local page.
type Upload struct {
	LocalID           string          `json:"local_id"`
	Title             string          `json:"title"`
	Summary           string          `json:"summary"`
	Markdown          string          `json:"markdown"`
	Details           json.RawMessage `json:"details,omitempty"`
	Links             []Link          `json:"links,omitempty"`
	SourceRefs        []SourceRef     `json:"source_refs,omitempty"`
	Attachments       []Attachment    `json:"attachments,omitempty"`
	RequestedScope    string          `json:"requested_scope"`
	GroupID           string          `json:"group_id,omitempty"`
	RedactionDeclared bool            `json:"redaction_declared"`
	UploadMode        string          `json:"upload_mode"`
	Client            Client          `json:"client"`
}

var (
	localIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	uploadModePattern = regexp.MustCompile(`^(manual|rule:[A-Za-z0-9._-]{1,64})$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sourceKinds       = map[string]bool{"file": true, "url": true, "case": true, "regulation": true, "other": true}
)

// ForbiddenDetailKeys must never reach the center: local redaction maps,
// credentials and the personal habit wiki stay on the device (contract surface).
var ForbiddenDetailKeys = []string{
	"redaction_map", "redaction_mapping", "desensitization_map", "脱敏映射", "脱敏映射表",
	"credential", "credentials", "password", "secret", "api_key", "access_token", "refresh_token", "private_key",
	"habits", "personal_habits", "习惯", "个人习惯",
}

func forbiddenKey(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, f := range ForbiddenDetailKeys {
		if k == f {
			return true
		}
	}
	return false
}

// findForbidden walks a decoded JSON value and returns dotted paths of forbidden keys.
func findForbidden(value any, path string, out *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			p := key
			if path != "" {
				p = path + "." + key
			}
			if forbiddenKey(key) {
				*out = append(*out, p)
			}
			findForbidden(child, p, out)
		}
	case []any:
		for _, child := range v {
			findForbidden(child, path, out)
		}
	}
}

// validLocalID accepts path-like local page ids (llm-wiki keeps pages in
// directories) but never empty or ".." segments.
func validLocalID(id string) bool {
	if len(id) > maxLocalIDBytes || !localIDPattern.MatchString(id) || strings.HasSuffix(id, "/") {
		return false
	}
	for _, segment := range strings.Split(id, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// Normalize trims fields in place and fills defaults that do not change meaning.
func Normalize(u *Upload) {
	u.LocalID = strings.TrimSpace(u.LocalID)
	u.Title = strings.TrimSpace(u.Title)
	u.Summary = strings.TrimSpace(u.Summary)
	u.GroupID = strings.TrimSpace(u.GroupID)
	u.RequestedScope = strings.TrimSpace(u.RequestedScope)
	u.UploadMode = strings.TrimSpace(u.UploadMode)
	if len(u.Details) == 0 || string(u.Details) == "null" {
		u.Details = json.RawMessage(`{}`)
	}
	if u.Links == nil {
		u.Links = []Link{}
	}
	if u.SourceRefs == nil {
		u.SourceRefs = []SourceRef{}
	}
	if u.Attachments == nil {
		u.Attachments = []Attachment{}
	}
}

// Validate returns every structural problem; group membership, DLP and blob
// presence are checked by the caller with server state.
func Validate(u Upload) []Reason {
	var r []Reason
	add := func(field, code, message string) { r = append(r, Reason{Field: field, Code: code, Message: message}) }

	if u.Client.SchemaVersion != SchemaVersion {
		add("client.schema_version", ReasonSchemaUnsupported, "this server accepts upload schema version 1")
	}
	if len(u.Client.Name) == 0 || len(u.Client.Name) > maxClientField || len(u.Client.Version) > maxClientField {
		add("client.name", ReasonInvalidFormat, "client name is required, client name and version at most 64 bytes")
	}
	switch {
	case u.LocalID == "":
		add("local_id", ReasonRequired, "stable local page id is required")
	case !validLocalID(u.LocalID):
		add("local_id", ReasonInvalidFormat, "local_id: 1-128 chars of A-Z a-z 0-9 . _ : - /, starting alphanumeric, no empty or .. segments")
	}
	switch n := utf8.RuneCountInString(u.Title); {
	case n == 0:
		add("title", ReasonRequired, "title is required")
	case n > maxTitleRunes:
		add("title", ReasonTooLong, "title is at most 160 characters")
	}
	if utf8.RuneCountInString(u.Summary) > maxSummaryRunes {
		add("summary", ReasonTooLong, "summary (one-sentence conclusion) is at most 300 characters")
	}
	switch {
	case strings.TrimSpace(u.Markdown) == "":
		add("markdown", ReasonRequired, "markdown body is required")
	case len(u.Markdown) > maxMarkdownBytes:
		add("markdown", ReasonTooLong, "markdown is at most 200000 bytes")
	}
	if len(u.Details) > maxDetailsBytes {
		add("details", ReasonTooLong, "details is at most 16384 bytes")
	} else {
		var details any
		if err := json.Unmarshal(u.Details, &details); err != nil {
			add("details", ReasonInvalidFormat, "details must be a JSON object")
		} else if _, ok := details.(map[string]any); !ok {
			add("details", ReasonInvalidFormat, "details must be a JSON object")
		} else {
			var hits []string
			findForbidden(details, "", &hits)
			for _, path := range hits {
				add("details."+path, ReasonForbiddenField, "this field must stay on the device and is never uploaded")
			}
		}
	}
	if !u.RedactionDeclared {
		add("redaction_declared", ReasonNotRedacted, "content must be redacted locally before upload")
	}
	switch u.RequestedScope {
	case ScopePersonal, ScopeOrganization:
		if u.GroupID != "" {
			add("group_id", ReasonInvalidFormat, "group_id is only allowed with team scope")
		}
	case ScopeTeam:
		if u.GroupID == "" {
			add("group_id", ReasonRequired, "team scope requires group_id")
		}
	case "":
		add("requested_scope", ReasonRequired, "requested_scope is personal, team or organization")
	default:
		add("requested_scope", ReasonInvalidFormat, "requested_scope is personal, team or organization")
	}
	if !uploadModePattern.MatchString(u.UploadMode) {
		add("upload_mode", ReasonInvalidFormat, "upload_mode is manual or rule:<rule id>")
	}
	if len(u.Links) > maxLinks {
		add("links", ReasonTooMany, "at most 200 links")
	}
	for i, l := range u.Links {
		if !validLocalID(l.TargetLocalID) || utf8.RuneCountInString(l.Label) > maxTitleRunes {
			add(indexed("links", i, "target_local_id"), ReasonInvalidFormat, "link target must be a valid local_id")
		}
	}
	if len(u.SourceRefs) > maxSourceRefs {
		add("source_refs", ReasonTooMany, "at most 100 source references")
	}
	for i, s := range u.SourceRefs {
		if !sourceKinds[s.Kind] || strings.TrimSpace(s.Ref) == "" || len(s.Ref) > maxRefBytes || utf8.RuneCountInString(s.Title) > maxTitleRunes {
			add(indexed("source_refs", i, "ref"), ReasonInvalidFormat, "source ref needs kind (file, url, case, regulation, other) and a ref up to 2048 bytes")
		}
	}
	if len(u.Attachments) > maxAttachments {
		add("attachments", ReasonTooMany, "at most 20 attachments")
	}
	for i, a := range u.Attachments {
		if !sha256Pattern.MatchString(a.SHA256) || strings.TrimSpace(a.Name) == "" || len(a.Name) > 255 || a.Size <= 0 || a.Size > maxAttachmentSize {
			add(indexed("attachments", i, "sha256"), ReasonInvalidFormat, "attachment needs lowercase hex sha256, a name and a size up to 50 MiB")
		}
	}
	return r
}

func indexed(list string, i int, field string) string {
	return list + "[" + strconv.Itoa(i) + "]." + field
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ContentHash fingerprints the meaning-bearing fields of a normalized upload.
func ContentHash(u Upload) string {
	var details any
	_ = json.Unmarshal(u.Details, &details) // re-encode: key order and spacing never change the hash
	return digest(struct {
		Title       string       `json:"title"`
		Summary     string       `json:"summary"`
		Markdown    string       `json:"markdown"`
		Details     any          `json:"details"`
		Links       []Link       `json:"links"`
		SourceRefs  []SourceRef  `json:"source_refs"`
		Attachments []Attachment `json:"attachments"`
	}{u.Title, u.Summary, u.Markdown, details, u.Links, u.SourceRefs, u.Attachments})
}

// RequestHash fingerprints the whole normalized request for idempotency.
func RequestHash(u Upload) string {
	return digest(struct {
		LocalID string `json:"local_id"`
		Content string `json:"content"`
		Scope   string `json:"scope"`
		GroupID string `json:"group_id"`
		Mode    string `json:"mode"`
		Client  Client `json:"client"`
	}{u.LocalID, ContentHash(u), u.RequestedScope, u.GroupID, u.UploadMode, u.Client})
}
