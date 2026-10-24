package contribution

import (
	"encoding/json"
	"strings"
	"testing"
)

func valid() Upload {
	u := Upload{
		LocalID: "wiki/equity-buyback", Title: "股权回购纠纷", Summary: "一句话结论", Markdown: "## 要点\n正文",
		Details:        json.RawMessage(`{"type":"办案经验","confidence":"已验证","source":"内部文件 2026-01"}`),
		Links:          []Link{{TargetLocalID: "wiki/related"}},
		SourceRefs:     []SourceRef{{Kind: "case", Ref: "(2025)沪01民终123号"}},
		RequestedScope: ScopeOrganization, RedactionDeclared: true, UploadMode: "manual",
		Client: Client{Name: "mindlink-desktop", Version: "0.1.0", SchemaVersion: 1},
	}
	Normalize(&u)
	return u
}

func codes(reasons []Reason) map[string]string {
	out := map[string]string{}
	for _, r := range reasons {
		out[r.Field] = r.Code
	}
	return out
}

func TestValidUploadHasNoReasons(t *testing.T) {
	if r := Validate(valid()); len(r) != 0 {
		t.Fatalf("unexpected reasons: %+v", r)
	}
}

func TestEveryProblemIsReportedWithFieldAndCode(t *testing.T) {
	u := valid()
	u.LocalID = "../escape"
	u.Title = ""
	u.Summary = strings.Repeat("长", 301)
	u.RedactionDeclared = false
	u.RequestedScope = ScopeTeam
	u.UploadMode = "auto"
	u.Client.SchemaVersion = 2
	u.SourceRefs = []SourceRef{{Kind: "email", Ref: "x"}}
	u.Attachments = []Attachment{{SHA256: "ABC", Name: "a.pdf", Size: 1}}
	got := codes(Validate(u))
	want := map[string]string{
		"local_id": ReasonInvalidFormat, "title": ReasonRequired, "summary": ReasonTooLong,
		"redaction_declared": ReasonNotRedacted, "group_id": ReasonRequired, "upload_mode": ReasonInvalidFormat,
		"client.schema_version": ReasonSchemaUnsupported, "source_refs[0].ref": ReasonInvalidFormat,
		"attachments[0].sha256": ReasonInvalidFormat,
	}
	for field, code := range want {
		if got[field] != code {
			t.Errorf("%s: got %q want %q (all: %v)", field, got[field], code, got)
		}
	}
}

func TestLocalIDsArePathLikeButContained(t *testing.T) {
	for id, ok := range map[string]bool{
		"wiki/equity-buyback": true, "a": true, "notes/2026/q4.md": true,
		"../escape": false, "wiki/../x": false, "wiki//x": false, "wiki/": false, "/abs": false, "wiki/./x": false,
	} {
		if validLocalID(id) != ok {
			t.Errorf("%q: got %v want %v", id, !ok, ok)
		}
	}
}

func TestForbiddenDetailKeysAreFoundAtAnyDepth(t *testing.T) {
	u := valid()
	u.Details = json.RawMessage(`{"type":"办案经验","meta":{"脱敏映射":{"张三":"当事人A"}},"items":[{"API_Key":"x"}]}`)
	got := codes(Validate(u))
	if got["details.meta.脱敏映射"] != ReasonForbiddenField || got["details.items.API_Key"] != ReasonForbiddenField {
		t.Fatalf("forbidden keys not reported: %v", got)
	}
	u.Details = json.RawMessage(`["not","an","object"]`)
	if codes(Validate(u))["details"] != ReasonInvalidFormat {
		t.Fatal("non-object details must be rejected")
	}
}

func TestHashesIgnoreFormattingButTrackMeaning(t *testing.T) {
	a, b := valid(), valid()
	b.Details = json.RawMessage(`{ "source":"内部文件 2026-01", "confidence":"已验证", "type":"办案经验" }`)
	if ContentHash(a) != ContentHash(b) {
		t.Fatal("detail key order or spacing must not change the content hash")
	}
	b.Markdown += "补充"
	if ContentHash(a) == ContentHash(b) {
		t.Fatal("a body change must change the content hash")
	}
	c := valid()
	c.UploadMode = "rule:weekly"
	if ContentHash(a) != ContentHash(c) || RequestHash(a) == RequestHash(c) {
		t.Fatal("upload mode is request identity, not content")
	}
}
