package identity

import (
	"slices"
	"strings"
	"testing"
)

func TestFromSecrets(t *testing.T) {
	if c, err := FromSecrets(nil); c != nil || err != nil {
		t.Fatalf("empty secrets must mean default identity, got %+v, %v", c, err)
	}
	if _, err := FromSecrets(map[string]string{TokenKey: "t"}); err == nil {
		t.Fatal("a secret without the passphrase must be rejected")
	}
	c, err := FromSecrets(map[string]string{TokenKey: "t", PassphraseKey: "p", "extra": "ignored"})
	if err != nil || c.Token != "t" || c.Passphrase != "p" {
		t.Fatalf("FromSecrets: %+v, %v", c, err)
	}
}

func TestFromEnvAndKey(t *testing.T) {
	if FromEnv([]string{"HOME=/x", PassphraseKey + "=p"}) != nil {
		t.Fatal("no token means no default identity")
	}
	a := FromEnv([]string{"HOME=/x", TokenKey + "=t", PassphraseKey + "=p"})
	b := &Credentials{Token: "t", Passphrase: "p"}
	if a == nil || a.Key() != b.Key() || len(a.Key()) != 16 {
		t.Fatalf("Key must be stable and short: %q vs %q", a.Key(), b.Key())
	}
	if (&Credentials{Token: "t", Passphrase: "other"}).Key() == b.Key() {
		t.Fatal("different passphrase must yield a different key")
	}
	if strings.Contains(a.Key(), "t") && strings.Contains(a.Key(), "p") && len(a.Key()) < 3 {
		t.Fatal("key must not leak inputs")
	}
}

func TestMergeEnv(t *testing.T) {
	base := []string{"HOME=/root", TokenKey + "=old", "PATH=/bin"}
	got := MergeEnv(base, map[string]string{TokenKey: "new", PassphraseKey: "p"})
	if !slices.Contains(got, TokenKey+"=new") || slices.Contains(got, TokenKey+"=old") {
		t.Fatalf("override not applied in place: %v", got)
	}
	if !slices.Contains(got, PassphraseKey+"=p") || !slices.Contains(got, "PATH=/bin") {
		t.Fatalf("missing appended or preserved variables: %v", got)
	}
	if !slices.Contains(base, TokenKey+"=old") {
		t.Fatal("base must not be modified")
	}
}
