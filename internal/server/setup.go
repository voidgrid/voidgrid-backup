package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

// Settings keys, and the recovery-code batch size. See docs/server.md's
// Authentication section: recovery codes are the way in if OIDC itself is
// ever unreachable, misconfigured, or simply not set up -- they
// deliberately don't depend on OIDC at all, only on the catalog.
const (
	settingSetupToken    = "setup_token"
	settingRecoveryCodes = "recovery_codes"
	settingOIDC          = "oidc"
	numRecoveryCodes     = 10
)

// OIDCConfig returns the OIDC settings saved through the setup wizard or
// the authentication settings page, or a zero (disabled) Config if none
// has been saved -- not necessarily what's actually in effect, since
// Controller.FlagOIDCConfig (env/flags) takes priority when given; see
// ReloadAuth.
func (c *Controller) OIDCConfig(ctx context.Context) (webauth.Config, error) {
	raw, err := c.Catalog.GetSetting(ctx, settingOIDC)
	if err != nil || raw == "" {
		return webauth.Config{}, err
	}
	var cfg webauth.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return webauth.Config{}, fmt.Errorf("stored OIDC settings: %w", err)
	}
	return cfg, nil
}

// SetOIDCConfig validates cfg (by actually constructing an Authenticator
// from it -- OIDC discovery included, so a typo'd issuer is caught right
// here, not only as a failed restart later), then saves it and installs it
// immediately: no restart needed. A config that fails to validate is never
// persisted. Callers must check FlagOIDCConfig first: this always saves to
// the catalog, which env/flags would otherwise keep overriding anyway.
func (c *Controller) SetOIDCConfig(ctx context.Context, cfg webauth.Config) error {
	a, err := webauth.New(ctx, cfg, c.SessionKey)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := c.Catalog.SetSetting(ctx, settingOIDC, string(b)); err != nil {
		return err
	}
	c.setAuth(a)
	return nil
}

type recoveryCode struct {
	Hash   string     `json:"hash"`
	UsedAt *time.Time `json:"used_at,omitempty"`
}

type recoveryCodeBatch struct {
	GeneratedAt time.Time      `json:"generated_at"`
	Codes       []recoveryCode `json:"codes"`
}

// SetupComplete reports whether recovery codes have ever been generated.
// Until they have, and only until they have, /setup is reachable without a
// session (see requireSetup in http.go and Authenticator.Middleware's own
// /setup exemption).
func (c *Controller) SetupComplete(ctx context.Context) (bool, error) {
	raw, err := c.Catalog.GetSetting(ctx, settingRecoveryCodes)
	return raw != "", err
}

// SetupToken returns the one-time token that must be presented to generate
// the first batch of recovery codes, creating it on first use. Presenting
// it is what stands in for "the operator, not whoever else reaches this
// server first" -- print it to the log at startup, the same way an agent's
// enrollment code works. It's meaningless once setup is complete.
func (c *Controller) SetupToken(ctx context.Context) (string, error) {
	tok, err := c.Catalog.GetSetting(ctx, settingSetupToken)
	if err != nil || tok != "" {
		return tok, err
	}
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return tok, c.Catalog.SetSetting(ctx, settingSetupToken, tok)
}

// GenerateRecoveryCodes creates a fresh batch of recovery codes, replacing
// any previous batch outright (every old code, used or not, stops
// working), and returns the plaintext codes -- shown to the operator
// exactly once, like a generated repository password.
func (c *Controller) GenerateRecoveryCodes(ctx context.Context) ([]string, error) {
	plain := make([]string, numRecoveryCodes)
	batch := recoveryCodeBatch{GeneratedAt: time.Now()}
	for i := range plain {
		code, err := newPassword()
		if err != nil {
			return nil, err
		}
		plain[i] = code
		batch.Codes = append(batch.Codes, recoveryCode{Hash: hashRecoveryCode(code)})
	}
	b, err := json.Marshal(batch)
	if err != nil {
		return nil, err
	}
	return plain, c.Catalog.SetSetting(ctx, settingRecoveryCodes, string(b))
}

// VerifyRecoveryCode checks code against the current batch. A match that
// hasn't been used yet is consumed (marked used) and returns true; a
// reused or unknown code returns false, never an error just for not
// matching.
func (c *Controller) VerifyRecoveryCode(ctx context.Context, code string) (bool, error) {
	raw, err := c.Catalog.GetSetting(ctx, settingRecoveryCodes)
	if err != nil || raw == "" {
		return false, err
	}
	var batch recoveryCodeBatch
	if err := json.Unmarshal([]byte(raw), &batch); err != nil {
		return false, err
	}
	h := hashRecoveryCode(code)
	matched := -1
	for i := range batch.Codes {
		if batch.Codes[i].UsedAt != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(batch.Codes[i].Hash), []byte(h)) == 1 {
			matched = i
			break
		}
	}
	if matched < 0 {
		return false, nil
	}
	now := time.Now()
	batch.Codes[matched].UsedAt = &now
	b, err := json.Marshal(batch)
	if err != nil {
		return false, err
	}
	return true, c.Catalog.SetSetting(ctx, settingRecoveryCodes, string(b))
}

// RemainingRecoveryCodes reports the size of the current batch and how
// many of it are still unused, for display on the setup/regenerate page.
// total is 0 if no batch has ever been generated.
func (c *Controller) RemainingRecoveryCodes(ctx context.Context) (total, remaining int, err error) {
	raw, err := c.Catalog.GetSetting(ctx, settingRecoveryCodes)
	if err != nil || raw == "" {
		return 0, 0, err
	}
	var batch recoveryCodeBatch
	if err := json.Unmarshal([]byte(raw), &batch); err != nil {
		return 0, 0, err
	}
	total = len(batch.Codes)
	for _, c := range batch.Codes {
		if c.UsedAt == nil {
			remaining++
		}
	}
	return total, remaining, nil
}

func hashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
