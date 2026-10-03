// Package enroll formats and parses agent enrollment codes.
//
// A code is "hbe1-<secret>-<pin>": secret is 20 random bytes (base32, lower
// case) and pin is the first 16 bytes of SHA-256 over the agent's bootstrap
// certificate (hex). The secret proves to the agent that the server was given
// the code; the pin lets the server verify it reached that agent and not
// something in between.
package enroll

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	prefix    = "hbe1"
	SecretLen = 20
	pinLen    = 16
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

var ErrMalformed = errors.New("malformed enrollment code")

// NewSecret returns a fresh random enrollment secret.
func NewSecret() ([]byte, error) {
	s := make([]byte, SecretLen)
	_, err := rand.Read(s)
	return s, err
}

// Format builds the code an operator copies from the agent to the server.
func Format(secret, bootstrapCertDER []byte) string {
	fp := sha256.Sum256(bootstrapCertDER)
	return prefix + "-" + strings.ToLower(b32.EncodeToString(secret)) + "-" + hex.EncodeToString(fp[:pinLen])
}

// Code is a parsed enrollment code.
type Code struct {
	Secret []byte
	Pin    []byte
}

// Parse validates and splits an enrollment code.
func Parse(s string) (Code, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 3 || parts[0] != prefix {
		return Code{}, ErrMalformed
	}
	secret, err := b32.DecodeString(strings.ToUpper(parts[1]))
	if err != nil || len(secret) != SecretLen {
		return Code{}, ErrMalformed
	}
	pin, err := hex.DecodeString(parts[2])
	if err != nil || len(pin) != pinLen {
		return Code{}, ErrMalformed
	}
	return Code{Secret: secret, Pin: pin}, nil
}

// Matches reports whether certDER is the certificate the code pins.
func (c Code) Matches(certDER []byte) bool {
	fp := sha256.Sum256(certDER)
	return subtle.ConstantTimeCompare(fp[:pinLen], c.Pin) == 1
}
