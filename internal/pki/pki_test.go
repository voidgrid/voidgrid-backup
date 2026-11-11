package pki

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"path/filepath"
	"testing"
	"time"
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

// writeShortCert replaces the certificate at path with one for the same key
// that ends after left, signed by parent (itself when parent is nil).
func writeShortCert(t *testing.T, path string, key *ecdsa.PrivateKey, isCA bool, parent *CA, left time.Duration) {
	t.Helper()
	old, err := ReadCert(path)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: old.Subject, DNSNames: old.DNSNames,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(left),
		IsCA: isCA, BasicConstraintsValid: isCA, KeyUsage: old.KeyUsage, ExtKeyUsage: old.ExtKeyUsage,
	}
	signer, signKey := tmpl, key
	if parent != nil {
		signer, signKey = parent.Cert, parent.Key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCert(path, der); err != nil {
		t.Fatal(err)
	}
}

// A CA close to its end gets a new certificate for the same key, so what it
// signed before still verifies.
func TestCARenewsBeforeItExpires(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	agentKey, _ := NewKey()
	leafDER, err := ca.SignAgentKey("a1", &agentKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	writeShortCert(t, filepath.Join(dir, "ca.crt"), ca.Key, true, nil, 30*24*time.Hour)

	renewed, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !renewed.Key.Equal(ca.Key) {
		t.Fatal("the CA key changed")
	}
	if time.Until(renewed.Cert.NotAfter) < 19*365*24*time.Hour {
		t.Fatalf("the CA still ends %s", renewed.Cert.NotAfter)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: renewed.Pool(), DNSName: AgentDNSName("a1"),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("a certificate issued before the renewal no longer verifies: %v", err)
	}
	again, err := LoadOrCreateCA(dir)
	if err != nil || !again.Cert.Equal(renewed.Cert) {
		t.Fatalf("a CA that is not due was renewed again: %v", err)
	}
}

// The server's own two certificates are replaced before they run out.
func TestServerCertificatesRenewBeforeTheyExpire(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := LoadOrCreateClientCert(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	writeShortCert(t, filepath.Join(dir, "server.crt"), client.PrivateKey.(*ecdsa.PrivateKey), false, ca, 30*24*time.Hour)
	client2, err := LoadOrCreateClientCert(dir, ca)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := x509.ParseCertificate(client2.Certificate[0])
	if time.Until(c2.NotAfter) < 9*365*24*time.Hour || c2.Subject.CommonName != ServerClientCN {
		t.Fatalf("client certificate after renewal: %s until %s", c2.Subject.CommonName, c2.NotAfter)
	}
	if _, err := c2.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}

	reg, err := LoadOrCreateServerTLSCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeShortCert(t, filepath.Join(dir, "registry.crt"), reg.PrivateKey.(*ecdsa.PrivateKey), false, nil, 24*time.Hour)
	reg2, err := LoadOrCreateServerTLSCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := x509.ParseCertificate(reg2.Certificate[0])
	if time.Until(r2.NotAfter) < 9*365*24*time.Hour {
		t.Fatalf("registration certificate still ends %s", r2.NotAfter)
	}
}
