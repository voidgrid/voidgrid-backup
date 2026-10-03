// Package agent implements the gRPC service each host runs.
//
// An agent starts unenrolled: it serves a self-signed bootstrap certificate,
// accepts only the enrollment RPCs, and logs an enrollment code. Once the
// server completes enrollment the agent requires the server's client
// certificate for every RPC and refuses enrollment.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/caps"
	"github.com/voidgrid/voidgrid-backup/internal/docker"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/version"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

// Files in the agent's data directory. agent.crt existing means enrolled.
const (
	fileKey       = "agent.key"
	fileBootstrap = "bootstrap.crt"
	fileSecret    = "enroll.secret"
	fileCert      = "agent.crt"
	fileCA        = "ca.crt"
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type Agent struct {
	agentpb.UnimplementedAgentServer

	engine *engine.Engine
	// LogBuf, if set, holds this process's recent log lines for the Logs RPC.
	LogBuf *logbuf.Buffer

	docker *docker.Client  // nil when no Docker socket is configured
	hv     virt.Hypervisor // nil when no libvirt socket is configured
	// opMu allows one backup or restore at a time on this host.
	opMu sync.Mutex

	dir          string
	hostname     string
	key          *ecdsa.PrivateKey
	bootstrap    tls.Certificate
	bootstrapDER []byte

	mu        sync.Mutex
	secret    []byte    // nil once enrolled
	pendingID string    // set by Enroll, consumed by CompleteEnrollment
	enrolled  *identity // nil until enrolled
}

type identity struct {
	id   string
	cert tls.Certificate
	cas  *x509.CertPool
}

// New loads the agent's state from dir, creating a key, bootstrap
// certificate and enrollment secret on first start.
func New(dir, hostname, dockerSocket string) (*Agent, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &Agent{dir: dir, hostname: hostname}
	if dockerSocket != "" {
		a.docker = docker.New(dockerSocket)
	}

	var err error
	if a.engine, err = engine.New(filepath.Join(dir, "kopia"), hostname); err != nil {
		return nil, err
	}
	if a.key, err = loadOrCreateKey(filepath.Join(dir, fileKey)); err != nil {
		return nil, err
	}
	if a.bootstrapDER, err = a.loadOrCreateBootstrap(); err != nil {
		return nil, err
	}
	a.bootstrap = pki.TLSCert(a.bootstrapDER, a.key)

	if _, err := os.Stat(a.path(fileCert)); err == nil {
		if a.enrolled, err = a.loadIdentity(); err != nil {
			return nil, err
		}
		return a, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if a.secret, err = os.ReadFile(a.path(fileSecret)); errors.Is(err, fs.ErrNotExist) {
		if a.secret, err = enroll.NewSecret(); err != nil {
			return nil, err
		}
		err = pki.WriteFileAtomic(a.path(fileSecret), a.secret, 0o600)
	}
	if err != nil {
		return nil, err
	}
	if len(a.secret) != enroll.SecretLen {
		return nil, fmt.Errorf("%s is corrupt; delete it to get a new enrollment code", a.path(fileSecret))
	}
	return a, nil
}

// EnrollmentCode returns the code to give the server, or "" once enrolled.
func (a *Agent) EnrollmentCode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enrolled != nil {
		return ""
	}
	return enroll.Format(a.secret, a.bootstrapDER)
}

// ID returns the server-assigned agent ID, or "" before enrollment.
func (a *Agent) ID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enrolled == nil {
		return ""
	}
	return a.enrolled.id
}

// TLSConfig picks the bootstrap or the enrolled identity per connection, so
// enrollment takes effect without a restart.
func (a *Agent) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			a.mu.Lock()
			id := a.enrolled
			a.mu.Unlock()
			if id == nil {
				return &tls.Config{
					MinVersion:   tls.VersionTLS13,
					NextProtos:   []string{"h2"},
					Certificates: []tls.Certificate{a.bootstrap},
				}, nil
			}
			return &tls.Config{
				MinVersion:            tls.VersionTLS13,
				NextProtos:            []string{"h2"},
				Certificates:          []tls.Certificate{id.cert},
				ClientAuth:            tls.RequireAndVerifyClientCert,
				ClientCAs:             id.cas,
				VerifyPeerCertificate: requireServerIdentity,
			}, nil
		},
	}
}

func requireServerIdentity(_ [][]byte, chains [][]*x509.Certificate) error {
	if len(chains) == 0 || chains[0][0].Subject.CommonName != pki.ServerClientCN {
		return errors.New("client certificate is not the voidgrid-backup server")
	}
	return nil
}

// Interceptor gates every RPC except enrollment behind a verified server
// client certificate. It also covers a connection opened before enrollment
// (bootstrap TLS, no client cert) that tries to call Ping afterwards.
func (a *Agent) Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	switch info.FullMethod {
	case agentpb.Agent_Enroll_FullMethodName, agentpb.Agent_CompleteEnrollment_FullMethodName:
		return handler(ctx, req) // these check the enrollment secret themselves
	}
	if a.ID() == "" {
		return nil, status.Error(codes.FailedPrecondition, "agent is not enrolled")
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "no peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return nil, status.Error(codes.Unauthenticated, "server client certificate required")
	}
	return handler(ctx, req)
}

func (a *Agent) Enroll(_ context.Context, req *agentpb.EnrollRequest) (*agentpb.EnrollResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.checkSecretLocked(req.GetSecret()); err != nil {
		return nil, err
	}
	if !idPattern.MatchString(req.GetAgentId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid agent ID")
	}
	csr, err := pki.CSR(a.key, req.GetAgentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create CSR: %v", err)
	}
	a.pendingID = req.GetAgentId()
	return &agentpb.EnrollResponse{CsrDer: csr, Hostname: a.hostname, Version: version.Version}, nil
}

func (a *Agent) CompleteEnrollment(_ context.Context, req *agentpb.CompleteEnrollmentRequest) (*agentpb.CompleteEnrollmentResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.checkSecretLocked(req.GetSecret()); err != nil {
		return nil, err
	}
	if a.pendingID == "" {
		return nil, status.Error(codes.FailedPrecondition, "call Enroll first")
	}
	ca, err := x509.ParseCertificate(req.GetCaDer())
	if err != nil || !ca.IsCA {
		return nil, status.Error(codes.InvalidArgument, "invalid CA certificate")
	}
	cert, err := x509.ParseCertificate(req.GetCertDer())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid agent certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   pki.AgentDNSName(a.pendingID),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent certificate does not verify: %v", err)
	}
	if cert.Subject.CommonName != a.pendingID || !a.key.PublicKey.Equal(cert.PublicKey) {
		return nil, status.Error(codes.InvalidArgument, "agent certificate is not for this agent")
	}

	// agent.crt last: its presence is what marks the agent enrolled.
	if err := pki.WriteCert(a.path(fileCA), req.GetCaDer()); err != nil {
		return nil, status.Errorf(codes.Internal, "save CA: %v", err)
	}
	if err := pki.WriteCert(a.path(fileCert), req.GetCertDer()); err != nil {
		return nil, status.Errorf(codes.Internal, "save certificate: %v", err)
	}
	if err := os.Remove(a.path(fileSecret)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove enrollment secret: %v", err)
	}
	a.enrolled = &identity{id: a.pendingID, cert: pki.TLSCert(req.GetCertDer(), a.key), cas: pool}
	a.secret, a.pendingID = nil, ""
	return &agentpb.CompleteEnrollmentResponse{}, nil
}

func (a *Agent) Ping(context.Context, *agentpb.PingRequest) (*agentpb.PingResponse, error) {
	resp := &agentpb.PingResponse{
		AgentId:  a.ID(),
		Hostname: a.hostname,
		Version:  version.Version,
		UnixTime: time.Now().Unix(),
		Uid:      int32(os.Geteuid()),
	}
	c, err := caps.Current()
	if err != nil {
		resp.MissingCapabilities = []string{"could not read capabilities: " + err.Error()}
	} else {
		resp.MissingCapabilities = c.Missing
	}
	return resp, nil
}

func (a *Agent) checkSecretLocked(got []byte) error {
	if a.enrolled != nil {
		return status.Error(codes.FailedPrecondition, "agent is already enrolled")
	}
	if subtle.ConstantTimeCompare(got, a.secret) != 1 {
		return status.Error(codes.PermissionDenied, "wrong enrollment secret")
	}
	return nil
}

func (a *Agent) path(name string) string { return filepath.Join(a.dir, name) }

func (a *Agent) loadOrCreateBootstrap() ([]byte, error) {
	if cert, err := pki.ReadCert(a.path(fileBootstrap)); err == nil {
		if !a.key.PublicKey.Equal(cert.PublicKey) {
			return nil, fmt.Errorf("%s does not match %s", a.path(fileBootstrap), a.path(fileKey))
		}
		return cert.Raw, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	der, err := pki.SelfSigned(a.key, "voidgrid-backup-agent bootstrap")
	if err != nil {
		return nil, err
	}
	return der, pki.WriteCert(a.path(fileBootstrap), der)
}

func (a *Agent) loadIdentity() (*identity, error) {
	cert, err := pki.ReadCert(a.path(fileCert))
	if err != nil {
		return nil, err
	}
	ca, err := pki.ReadCert(a.path(fileCA))
	if err != nil {
		return nil, err
	}
	if !a.key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%s does not match %s", a.path(fileCert), a.path(fileKey))
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &identity{id: cert.Subject.CommonName, cert: pki.TLSCert(cert.Raw, a.key), cas: pool}, nil
}

func loadOrCreateKey(path string) (*ecdsa.PrivateKey, error) {
	key, err := pki.ReadKey(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	if key, err = pki.NewKey(); err != nil {
		return nil, err
	}
	return key, pki.WriteKey(path, key)
}
