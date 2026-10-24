package dlp

import "testing"

// Valid GB 11643 specimen: computed check digit must match.
func TestResidentIDValid(t *testing.T) {
	// 11010519491231002X is the widely used GB example.
	if !ResidentIDValid("11010519491231002X") {
		t.Fatalf("known-valid resident ID rejected")
	}
	if ResidentIDValid("110105194912310021") {
		t.Fatalf("permuted check digit accepted")
	}
	if ResidentIDValid("1101051949123100") {
		t.Fatalf("short id accepted")
	}
}

func TestBankCardValid(t *testing.T) {
	// 6222600260001072444 — Luhn-valid specimen.
	if !BankCardValid("6222600260001072444") {
		t.Fatalf("Luhn-valid card rejected")
	}
	if BankCardValid("6222600260001072445") {
		t.Fatalf("Luhn-invalid card accepted")
	}
}

func TestUSCCValid(t *testing.T) {
	// 91350100M000100Y43 — GB 32100 example (check char 3).
	if !USCCValid("91350100M000100Y43") {
		t.Fatalf("valid USCC rejected")
	}
	if USCCValid("91350100M000100Y4A") {
		t.Fatalf("invalid check char accepted")
	}
	if USCCValid("91350100M000100Y4") {
		t.Fatalf("short USCC accepted")
	}
}
