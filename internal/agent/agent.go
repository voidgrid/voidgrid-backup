// Package agent implements the gRPC service each host runs.
//
// An agent starts unenrolled: it generates a key and a self-signed
// certificate, then registers with the server (see Register) using the token
// from its configuration. Once an operator approves it, the server issues a
// certificate for that key and the agent requires the server's client
// certificate for every RPC.
package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	fileCert      = "agent.crt"
	fileCA        = "ca.crt"
)

type Agent struct {
	agentpb.UnimplementedAgentServer

	// engine is created once the agent has an identity: its Kopia host name
	// is the server-assigned agent ID. Nothing reaches it before that, since
	// Interceptor refuses every RPC until the agent is enrolled.
	engine *engine.Engine
	// LogBuf, if set, holds this process's recent log lines for the Logs RPC.
	LogBuf *logbuf.Buffer

	docker *docker.Client  // nil when no Docker socket is configured
	hv     virt.Hypervisor // nil when no libvirt socket is configured
	// opMu allows one backup or restore at a time on this host.
	opMu sync.Mutex

	dir          string
	hostname     string // the OS host name, only a suggestion for the operator
	key          *ecdsa.PrivateKey
	bootstrap    tls.Certificate
	bootstrapDER []byte

	mu       sync.Mutex
	enrolled *identity // nil until enrolled
}

type identity struct {
	id   string
	cert tls.Certificate
	cas  *x509.CertPool
}

// New loads the agent's state from dir, creating a key and a self-signed
// certificate on first start.
func New(dir, dockerSocket string) (*Agent, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	a := &Agent{dir: dir, hostname: host}
	if dockerSocket != "" {
		a.docker = docker.New(dockerSocket)
	}

	var err error
	if a.key, err = loadOrCreateKey(filepath.Join(dir, fileKey)); err != nil {
		return nil, err
	}
	if a.bootstrapDER, err = a.loadOrCreateBootstrap(); err != nil {
		return nil, err
	}
	a.bootstrap = pki.TLSCert(a.bootstrapDER, a.key)

	if _, err := os.Stat(a.path(fileCert)); err == nil {
		id, err := a.loadIdentity()
		if err != nil {
			return nil, err
		}
		if a.engine, err = engine.New(a.engineDir(), id.id); err != nil {
			return nil, err
		}
		a.enrolled = id
		return a, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return a, nil
}

func (a *Agent) engineDir() string { return filepath.Join(a.dir, "kopia") }

// Fingerprint is the SHA-256 (hex) of the agent's self-signed certificate:
// what the server's Agents page shows for a pending registration, so an
// operator can check they are approving this agent and not an impostor that
// also has the token.
func (a *Agent) Fingerprint() string {
	return hex.EncodeToString(pki.Fingerprint(a.bootstrapDER))
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
				MinVersion:   tls.VersionTLS13,
				NextProtos:   []string{"h2"},
				Certificates: []tls.Certificate{id.cert},
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    id.cas,
				// VerifyConnection, not VerifyPeerCertificate: the latter
				// is skipped on resumed sessions, this runs on every one.
				VerifyConnection: requireServerIdentity,
			}, nil
		},
	}
}

func requireServerIdentity(cs tls.ConnectionState) error {
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 ||
		cs.VerifiedChains[0][0].Subject.CommonName != pki.ServerClientCN {
		return errors.New("client certificate is not the voidgrid-backup server")
	}
	return nil
}

// Interceptor gates every RPC behind a verified server client certificate. It
// also covers a connection opened before enrollment (bootstrap TLS, no client
// cert) that tries to call an RPC afterwards.
func (a *Agent) Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
	start := time.Now()
	resp, err := handler(ctx, req)
	logRPC(info.FullMethod, time.Since(start), req, resp, err)
	return resp, err
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
