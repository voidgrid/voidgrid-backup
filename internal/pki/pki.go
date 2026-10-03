// Package pki holds the server's private CA and the certificate plumbing for
// agent enrollment. Keys are ECDSA P-256; files are PEM, written atomically.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ServerClientCN is the common name of the server's client certificate.
// Agents accept only this identity once enrolled.
const ServerClientCN = "voidgrid-backup-server"

// AgentDNSName is the SAN an agent certificate carries and the name the
// server verifies when it dials that agent. Agents are usually dialed by IP,
// so the name is synthetic.
func AgentDNSName(agentID string) string {
	return agentID + ".agent.voidgrid-backup.invalid"
}

// CA is the server's private certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// Pool returns a pool containing only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// LoadOrCreateCA loads ca.crt/ca.key from dir, creating them on first use.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if exists, err := fileExists(certPath); err != nil {
		return nil, err
	} else if exists {
		cert, err := ReadCert(certPath)
		if err != nil {
			return nil, err
		}
		key, err := ReadKey(keyPath)
		if err != nil {
			return nil, err
		}
		if !key.PublicKey.Equal(cert.PublicKey) {
			return nil, fmt.Errorf("%s does not match %s", keyPath, certPath)
		}
		return &CA{Cert: cert, Key: key}, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "voidgrid-backup CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(20, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	// Key first: the cert's presence is what marks the CA as complete.
	if err := WriteKey(keyPath, key); err != nil {
		return nil, err
	}
	if err := WriteCert(certPath, der); err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// SignAgentCSR issues an agent certificate for agentID. The CSR's key must be
// the key pinned from the agent's bootstrap certificate during enrollment.
func (ca *CA) SignAgentCSR(csrDER []byte, agentID string, pinned crypto.PublicKey) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	if csr.Subject.CommonName != agentID {
		return nil, fmt.Errorf("CSR is for %q, expected %q", csr.Subject.CommonName, agentID)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(pinned) {
		return nil, errors.New("CSR key does not match the pinned bootstrap certificate")
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agentID},
		DNSNames:     []string{AgentDNSName(agentID)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
}

// LoadOrCreateClientCert loads the server's client certificate
// (server.crt/server.key in dir), issuing it from ca on first use.
func LoadOrCreateClientCert(dir string, ca *CA) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if exists, err := fileExists(certPath); err != nil {
		return tls.Certificate{}, err
	} else if exists {
		return tls.LoadX509KeyPair(certPath, keyPath)
	}

	key, err := NewKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := newSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ServerClientCN},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := WriteKey(keyPath, key); err != nil {
		return tls.Certificate{}, err
	}
	if err := WriteCert(certPath, der); err != nil {
		return tls.Certificate{}, err
	}
	return TLSCert(der, key), nil
}

// NewKey generates an ECDSA P-256 key.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// SelfSigned returns a self-signed server certificate for key.
func SelfSigned(key *ecdsa.PrivateKey, cn string) ([]byte, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
}

// CSR returns a DER certificate request for key with the given common name.
func CSR(key *ecdsa.PrivateKey, cn string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
}

// TLSCert pairs a DER certificate with its key.
func TLSCert(certDER []byte, key *ecdsa.PrivateKey) tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}
}

// Fingerprint is the SHA-256 of a DER certificate.
func Fingerprint(der []byte) []byte {
	sum := sha256.Sum256(der)
	return sum[:]
}

func WriteKey(path string, key *ecdsa.PrivateKey) error {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func ReadKey(path string) (*ecdsa.PrivateKey, error) {
	der, err := readPEM(path, "EC PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(der)
}

func WriteCert(path string, der []byte) error {
	return WriteFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func ReadCert(path string) (*x509.Certificate, error) {
	der, err := readPEM(path, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// WriteFileAtomic writes data to a temp file in path's directory, syncs it,
// and renames it over path, so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readPEM(path, typ string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != typ {
		return nil, fmt.Errorf("%s: no %s PEM block", path, typ)
	}
	return blk.Bytes, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}
