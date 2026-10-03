package enroll

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	cert := []byte("server cert DER")
	tok, err := Parse(" " + Format(secret, cert) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tok.Secret, secret) {
		t.Fatal("secret did not round-trip")
	}
	if !tok.Matches(cert) {
		t.Fatal("pin does not match the certificate it was made from")
	}
	if tok.Matches([]byte("some other cert")) {
		t.Fatal("pin matches a different certificate")
	}
}

func TestParseRejects(t *testing.T) {
	good := Format(make([]byte, SecretLen), []byte("x"))
	for _, s := range []string{
		"",
		"vbr1",
		"vbr2" + good[4:],
		good + "-extra",
		"vbr1-!!!-" + good[len(good)-32:],
		good[:len(good)-2],
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted a malformed token", s)
		}
	}
}
