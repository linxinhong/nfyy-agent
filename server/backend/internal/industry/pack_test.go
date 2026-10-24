package industry

import (
	"os"
	"testing"
)

func TestLegalPackCoversAllTypesAndValidatesSubmission(t *testing.T) {
	files := os.DirFS("../../industries")
	pack, err := Load(files, "legal")
	if err != nil {
		t.Fatal(err)
	}
	if pack.ID != "legal" || len(pack.KnowledgeTypes) != 6 {
		t.Fatalf("unexpected legal pack: %+v", pack)
	}
	for _, kind := range pack.KnowledgeTypes {
		markdown, err := Template(files, "legal", kind.ID)
		if err != nil || markdown == "" {
			t.Fatalf("missing %s template: %v", kind.ID, err)
		}
	}
	in := Submission{
		Title: "股权纠纷中的证据保全", Type: "case_experience", Author: "示例律师", SubmittedAt: "2026-10-15",
		Area: "诉讼", Source: "内部文件-001", Confidence: "已验证", Redacted: true,
		Fields:   map[string]string{"cause": "股权纠纷", "instance": "一审", "region": "上海"},
		Sections: map[string]string{"conclusion": "尽早保全", "background": "已脱敏案例", "points": "步骤", "citations": "无", "limits": "限于本地区"},
	}
	if got := pack.Validate(in); len(got) != 0 {
		t.Fatalf("valid submission rejected: %+v", got)
	}
	in.Source = ""
	in.Redacted = false
	in.Fields["region"] = ""
	in.Sections["limits"] = ""
	got := pack.Validate(in)
	fields := map[string]bool{}
	for _, issue := range got {
		fields[issue.Field] = true
	}
	for _, required := range []string{"source", "redacted", "fields.region", "sections.limits"} {
		if !fields[required] {
			t.Errorf("missing validation error for %s: %+v", required, got)
		}
	}
}
