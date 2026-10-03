// Package enroll formats and parses agent registration tokens.
//
// A token is "vbr1-<secret>-<pin>": secret is 20 random bytes (base32, lower
// case) and pin is the first 16 bytes of SHA-256 over the server's
// registration-channel certificate (hex). The secret proves to the server that
// the agent was given the token; the pin lets the agent verify it reached
// that server and not something in between, with no CA or domain name
// involved.
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
	prefix    = "vbr1"
	SecretLen = 20
	pinLen    = 16
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

var ErrMalformed = errors.New("malformed registration token")

// NewSecret returns a fresh random registration secret.
func NewSecret() ([]byte, error) {
	s := make([]byte, SecretLen)
	_, err := rand.Read(s)
	return s, err
}

// Format builds the token an operator copies from the server to an agent.
func Format(secret, serverCertDER []byte) string {
	fp := sha256.Sum256(serverCertDER)
	return prefix + "-" + strings.ToLower(b32.EncodeToString(secret)) + "-" + hex.EncodeToString(fp[:pinLen])
}

// Token is a parsed registration token.
type Token struct {
	Secret []byte
	Pin    []byte
}

// Parse validates and splits a registration token.
func Parse(s string) (Token, error) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) != 3 || parts[0] != prefix {
		return Token{}, ErrMalformed
	}
	secret, err := b32.DecodeString(strings.ToUpper(parts[1]))
	if err != nil || len(secret) != SecretLen {
		return Token{}, ErrMalformed
	}
	pin, err := hex.DecodeString(parts[2])
	if err != nil || len(pin) != pinLen {
		return Token{}, ErrMalformed
	}
	return Token{Secret: secret, Pin: pin}, nil
}

// Matches reports whether certDER is the certificate the token pins.
func (t Token) Matches(certDER []byte) bool {
	fp := sha256.Sum256(certDER)
	return subtle.ConstantTimeCompare(fp[:pinLen], t.Pin) == 1
}
