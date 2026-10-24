package dlp

// Checksum validators (TASK-030): regex candidates for identity-number
// rules must pass their national checksum before counting as hits. This
// kills the digit-permutation false positives plain regex produces.

import "strings"

// ValidatorFor maps rule names to their national checksum validators.
// Unknown names get none (plain regex semantics).
func ValidatorFor(ruleName string) func(string) bool {
	switch ruleName {
	case "cn_resident_id":
		return ResidentIDValid
	case "cn_bank_card":
		return BankCardValid
	case "cn_uscc":
		return USCCValid
	default:
		return nil
	}
}

// ResidentIDValid checks an 18-digit Chinese resident ID (GB 11643):
// 6-digit region + 8-digit birthdate + 3 sequence + 1 check digit. The
// check digit is Z = (w·a) mod 11 mapped 0-9,X per the standard weights.
func ResidentIDValid(id string) bool {
	if len(id) != 18 {
		return false
	}
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	sum := 0
	for i := 0; i < 17; i++ {
		d := int(id[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		sum += d * weights[i]
	}
	var want byte
	switch sum % 11 {
	case 0:
		want = '1'
	case 1:
		want = '0'
	case 2:
		want = 'X'
	case 3:
		want = '9'
	case 4:
		want = '8'
	case 5:
		want = '7'
	case 6:
		want = '6'
	case 7:
		want = '5'
	case 8:
		want = '4'
	case 9:
		want = '3'
	case 10:
		want = '2'
	}
	last := id[17]
	if last >= 'a' && last <= 'z' {
		last -= 0x20 // lowercase x accepted
	}
	return last == want
}

// BankCardValid checks a 13-19 digit bank card number with the Luhn
// algorithm.
func BankCardValid(card string) bool {
	if len(card) < 13 || len(card) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(card) - 1; i >= 0; i-- {
		d := int(card[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// usccWeights is the GB 32100 unified social credit code check table.
var usccChars = "0123456789ABCDEFGHJKLMNPQRTUWXY"
var usccWeights = [17]int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}

// USCCValid checks an 18-character unified social credit code: the last
// character is the GB 32100 check character C = 31 - (w·c mod 31) mod 31
// (with 0 mapped to 0).
func USCCValid(code string) bool {
	if len(code) != 18 {
		return false
	}
	up := strings.ToUpper(code)
	sum := 0
	for i := 0; i < 17; i++ {
		v := strings.IndexByte(usccChars, up[i])
		if v < 0 {
			return false
		}
		sum += v * usccWeights[i]
	}
	want := (31 - sum%31) % 31
	return up[17] == usccChars[want]
}

// keepDigits strips separators a scanner may have included in a candidate.
func keepDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
