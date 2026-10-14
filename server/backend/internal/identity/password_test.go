package identity

import "testing"

func TestPasswordHash(t *testing.T) {
	hash, err := HashPassword("long passphrase for a member")
	if err != nil || hash == "long passphrase for a member" || !VerifyPassword(hash, "long passphrase for a member") {
		t.Fatal("password hash cannot be verified")
	}
	if VerifyPassword(hash, "incorrect passphrase") || VerifyPassword("broken", "long passphrase for a member") {
		t.Fatal("invalid password accepted")
	}
	if _, err := HashPassword("too short"); err == nil {
		t.Fatal("short password accepted")
	}
}
