package execution

import (
	"strings"
	"testing"
)

func TestValidateNamespaceRejectsEmpty(t *testing.T) {
	if err := ValidateNamespace(""); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("empty namespace: err = %v", err)
	}
}

func TestValidateNamespaceRejectsReserved(t *testing.T) {
	if err := ValidateNamespace("legacy"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("legacy namespace: err = %v", err)
	}
}

func TestValidateNamespaceRejectsControlChars(t *testing.T) {
	for _, ns := range []string{"a\nb", "a\tb", "a\x00b", "a\x7fb"} {
		if err := ValidateNamespace(ns); err == nil || !strings.Contains(err.Error(), "control") {
			t.Fatalf("namespace %q: err = %v", ns, err)
		}
	}
}

func TestValidateNamespaceAcceptsCommon(t *testing.T) {
	for _, ns := range []string{"acme", "acme-prod", "ns/2026", "客户-01", "Acme.MixCase"} {
		if err := ValidateNamespace(ns); err != nil {
			t.Fatalf("namespace %q: unexpected error %v", ns, err)
		}
	}
}

func TestValidateNamespaceNoNormalization(t *testing.T) {
	// Case differences are distinct namespaces; no case folding.
	if err := ValidateNamespace("Acme"); err != nil {
		t.Fatalf("Acme: %v", err)
	}
	if err := ValidateNamespace("acme"); err != nil {
		t.Fatalf("acme: %v", err)
	}
}

func TestScopeHelpers(t *testing.T) {
	scope := NamespaceScope{Namespace: "acme"}
	if !scope.HasNamespace() {
		t.Fatalf("acme scope should be valid")
	}
	if scope.String() != "acme" {
		t.Fatalf("scope string = %q", scope.String())
	}
	if (NamespaceScope{}).HasNamespace() {
		t.Fatalf("empty scope must be invalid")
	}
	if (NamespaceScope{Namespace: "legacy"}).HasNamespace() {
		t.Fatalf("legacy scope must be invalid for managed use")
	}
}

func TestNamespaceLengthGuard(t *testing.T) {
	long := strings.Repeat("n", MaxNamespaceRunes+1)
	if err := ValidateNamespaceLength(long); err == nil {
		t.Fatalf("expected length rejection")
	}
	if err := ValidateNamespaceLength(strings.Repeat("n", MaxNamespaceRunes)); err != nil {
		t.Fatalf("max-length namespace rejected: %v", err)
	}
}

func TestPrincipalCarriesPartition(t *testing.T) {
	p := Principal{Subject: "alice", Namespace: "acme", Issuer: "idp", AuthzEpoch: 7}
	if p.Namespace != "acme" || p.Subject != "alice" || p.AuthzEpoch != 7 {
		t.Fatalf("principal = %+v", p)
	}
}
