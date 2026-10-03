package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// clientCert issues a client certificate from ca with the given common name.
func clientCert(t *testing.T, ca *pki.CA, cn string) tls.Certificate {
	t.Helper()
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	return pki.TLSCert(der, key)
}

func TestOnlyTheServerIdentityIsAccepted(t *testing.T) {
	ca, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	certDER, err := ca.SignAgentKey("a1", &a.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.finishEnrollment("a1", certDER, ca.Cert.Raw); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(a.TLSConfig())), grpc.UnaryInterceptor(a.Interceptor))
	agentpb.RegisterAgentServer(s, a)
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	ping := func(cert tls.Certificate) error {
		conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			RootCAs:      ca.Pool(),
			ServerName:   pki.AgentDNSName("a1"),
		})))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = agentpb.NewAgentClient(conn).Ping(ctx, &agentpb.PingRequest{})
		return err
	}

	server, err := pki.LoadOrCreateClientCert(t.TempDir(), ca)
	if err != nil {
		t.Fatal(err)
	}
	if err := ping(server); err != nil {
		t.Fatalf("the server's client certificate was refused: %v", err)
	}
	// Signed by the same CA, but not the server's identity.
	if err := ping(clientCert(t, ca, "someone-else")); err == nil {
		t.Fatal("a CA-signed client certificate with another name was accepted")
	}
}
