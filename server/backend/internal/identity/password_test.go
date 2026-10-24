package identity

import "testing"

func TestPasswordHash(t *testing.T) {
	hash, err := HashPassword("demo@yuanpu")
	if err != nil || hash == "demo@yuanpu" || !VerifyPassword(hash, "demo@yuanpu") {
		t.Fatal("password hash cannot be verified")
	}
	if VerifyPassword(hash, "incorrect passphrase") || VerifyPassword("broken", "demo@yuanpu") {
		t.Fatal("invalid password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}
