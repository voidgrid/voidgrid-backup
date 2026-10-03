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

func TestSignAgentKey(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewKey()

	der, err := ca.SignAgentKey("a1", &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "a1" || !key.PublicKey.Equal(cert.PublicKey) {
		t.Fatalf("certificate is for CN=%q or the wrong key", cert.Subject.CommonName)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     ca.Pool(),
		DNSName:   AgentDNSName("a1"),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("agent cert does not verify: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: AgentDNSName("a2"), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("certificate for a1 verified as a2")
	}
	if _, err := ca.SignAgentKey("a1", "not a key"); err == nil {
		t.Fatal("signed something that is not an ECDSA key")
	}
}

func TestServerTLSCertPersists(t *testing.T) {
	dir := t.TempDir()
	c1, err := LoadOrCreateServerTLSCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := LoadOrCreateServerTLSCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Fatal("reloaded registration cert differs")
	}
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	if leaf.Issuer.String() != leaf.Subject.String() {
		t.Fatal("registration cert should be self-signed")
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
