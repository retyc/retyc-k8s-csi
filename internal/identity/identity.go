// Package identity models one Retyc identity (offline token + AGE key passphrase) and where it
// comes from: the driver's own environment (cluster-wide default) or the per-call CSI secrets
// that kubelet and external-provisioner resolve from a StorageClass's secret parameters.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Environment variable names the retyc CLI reads; also the keys expected in a credentials Secret.
const (
	TokenKey      = "RETYC_TOKEN"
	PassphraseKey = "RETYC_KEY_PASSPHRASE" //nolint:gosec // G101: a variable name, not a credential
)

// Credentials is one Retyc identity.
type Credentials struct {
	Token      string
	Passphrase string
}

// FromSecrets extracts credentials from a CSI request's Secrets map. An empty map means "use the
// driver's default identity" and yields nil, nil; a map with only one of the two keys is a
// misconfigured Secret and is an error.
func FromSecrets(secrets map[string]string) (*Credentials, error) {
	if len(secrets) == 0 {
		return nil, nil //nolint:nilnil // nil credentials mean "default identity", a valid outcome
	}
	c := &Credentials{Token: secrets[TokenKey], Passphrase: secrets[PassphraseKey]}
	if c.Token == "" || c.Passphrase == "" {
		return nil, fmt.Errorf("credentials secret must contain both %s and %s", TokenKey, PassphraseKey)
	}

	return c, nil
}

// FromEnv extracts the default identity from a process environment (os.Environ() format), or
// nil when RETYC_TOKEN is not set.
func FromEnv(env []string) *Credentials {
	c := &Credentials{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case TokenKey:
			c.Token = v
		case PassphraseKey:
			c.Passphrase = v
		}
	}
	if c.Token == "" {
		return nil
	}

	return c
}

// Key is a short, stable, non-reversible identifier of the credentials, used to name the
// per-identity WebDAV server and state directory. Never log the inputs; the key is safe to log.
func (c *Credentials) Key() string {
	sum := sha256.Sum256([]byte(c.Token + "\x00" + c.Passphrase))

	return hex.EncodeToString(sum[:8])
}

// EnvOverrides returns the variables to set for a retyc subprocess using these credentials.
func (c *Credentials) EnvOverrides() map[string]string {
	return map[string]string{TokenKey: c.Token, PassphraseKey: c.Passphrase}
}

// MergeEnv returns base (os.Environ() format) with overrides applied: an existing variable is
// replaced in place, a new one appended. base is not modified.
func MergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]bool, len(overrides))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if v, ok := overrides[k]; ok {
			out = append(out, k+"="+v)
			seen[k] = true

			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}

	return out
}
