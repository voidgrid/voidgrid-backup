package pki

import (
	"crypto/x509"
	"testing"
)

func TestCAPersists(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ca1.Cert.Equal(ca2.Cert) {
		t.Fatal("reloaded CA differs from the one created")
	}
}

func TestSignAgentCSR(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewKey()
	other, _ := NewKey()
	csr, err := CSR(key, "a1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ca.SignAgentCSR(csr, "a1", &other.PublicKey); err == nil {
		t.Fatal("signed a CSR whose key does not match the pinned key")
	}
	if _, err := ca.SignAgentCSR(csr, "a2", &key.PublicKey); err == nil {
		t.Fatal("signed a CSR for the wrong agent ID")
	}

	der, err := ca.SignAgentCSR(csr, "a1", &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		DNSName:   AgentDNSName("a1"),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("agent cert does not verify: %v", err)
	}
}

func TestClientCertPersists(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	c1, err := LoadOrCreateClientCert(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreateClientCert(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Fatal("reloaded client cert differs")
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	if leaf.Subject.CommonName != ServerClientCN {
		t.Fatalf("client cert CN = %q", leaf.Subject.CommonName)
	}
}
