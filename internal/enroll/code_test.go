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
	cert := []byte("bootstrap cert DER")
	c, err := Parse(" " + Format(secret, cert) + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Secret, secret) {
		t.Fatal("secret did not round-trip")
	}
	if !c.Matches(cert) {
		t.Fatal("pin does not match the certificate it was made from")
	}
	if c.Matches([]byte("some other cert")) {
		t.Fatal("pin matches a different certificate")
	}
}

func TestParseRejects(t *testing.T) {
	good := Format(make([]byte, SecretLen), []byte("x"))
	for _, s := range []string{
		"",
		"hbe1",
		"hbe2" + good[4:],
		good + "-extra",
		"hbe1-!!!-" + good[len(good)-32:],
		good[:len(good)-2],
	} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted a malformed code", s)
		}
	}
}
