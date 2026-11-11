package agent

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/safeint"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
	"github.com/voidgrid/voidgrid-backup/internal/version"
)

// RegisterConfig says where and how to register.
type RegisterConfig struct {
	// Server is the host:port of the server's agent registration listener.
	Server string
	// Token is the registration token copied from the server.
	Token string
	// ListenPort is the port this agent's gRPC server listens on, which the
	// server dials after approval.
	ListenPort int
	// PollEvery is how often to ask for a decision; 0 means 5 seconds.
	PollEvery time.Duration
	// MaxBackoff caps the wait after a failed attempt; 0 means 5 minutes.
	MaxBackoff time.Duration
}

// Register contacts the server, then waits for an operator to approve the
// agent and installs the certificate it issues. It returns nil once the agent
// is enrolled (immediately if it already was) and an error only if ctx ends or
// the token is unusable. Connection failures and rejections are logged and
// retried.
func (a *Agent) Register(ctx context.Context, cfg RegisterConfig) error {
	if a.ID() != "" {
		return nil
	}
	tok, err := enroll.Parse(cfg.Token)
	if err != nil {
		return fmt.Errorf("VB_TOKEN: %w", err)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 5 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}

	backoff := cfg.PollEvery
	wait := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	registered, rejectedLogged := false, false
	for {
		done, err := a.registerOnce(ctx, cfg, tok, &registered, &rejectedLogged)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			slog.Warn("registration with the server failed; will retry", "server", cfg.Server, "err", err, "retry_in", backoff.String())
			if werr := wait(backoff); werr != nil {
				return werr
			}
			if backoff *= 2; backoff > cfg.MaxBackoff {
				backoff = cfg.MaxBackoff
			}
		case done:
			return nil
		default:
			backoff = cfg.PollEvery
			if werr := wait(cfg.PollEvery); werr != nil {
				return werr
			}
		}
	}
}

// registerOnce makes one connection: Register if needed, then Poll. done is
// true once the certificate has been installed.
func (a *Agent) registerOnce(ctx context.Context, cfg RegisterConfig, tok enroll.Token, registered, rejectedLogged *bool) (done bool, err error) {
	// This connection is verified, by pinning, not by a certificate authority.
	//
	// InsecureSkipVerify is set because Go's normal check cannot apply here: the
	// server's registration certificate is self-signed and has no name, and an
	// agent that has not enrolled yet has no CA and no domain to check it against.
	// What replaces that check is VerifyConnection below. It runs after the
	// handshake and refuses the connection unless the certificate the server
	// presented hashes to the pin inside the registration token the operator
	// copied from the server (see package enroll). A server that does not hold
	// that exact certificate, so a wrong host or someone in the middle, is
	// refused. Skipping the standard check with nothing in its place would let
	// anyone impersonate the server; crypto/tls says InsecureSkipVerify is meant
	// "only for testing or in combination with VerifyConnection or
	// VerifyPeerCertificate", and that VerifyConnection runs for every connection,
	// resumptions included, whatever InsecureSkipVerify says.
	//
	// The security linter (gosec G402) flags InsecureSkipVerify and cannot see
	// VerifyConnection, so .golangci.yml excludes that one finding for this one
	// file, and for nothing else: do not widen it, and do not remove
	// VerifyConnection. Once the agent is approved this path is never used again:
	// it authenticates with the certificate the server issued it.
	conn, err := grpc.NewClient(cfg.Server, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{a.bootstrap},
		InsecureSkipVerify: true, // see above: VerifyConnection pins the server's certificate instead
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !tok.Matches(cs.PeerCertificates[0].Raw) {
				return errors.New("the server's certificate does not match the token (wrong server, or a token from another install)")
			}
			return nil
		},
	})))
	if err != nil {
		return false, err
	}
	defer tidy.CloseWarn("registration connection", conn)
	client := agentpb.NewRegistryClient(conn)

	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 15*time.Second) }
	if !*registered {
		cctx, cancel := call()
		_, err := client.Register(cctx, &agentpb.RegisterRequest{
			Secret: tok.Secret, Hostname: a.hostname, Version: version.Version, ListenPort: safeint.Int32(cfg.ListenPort),
		})
		cancel()
		if err != nil {
			return false, err
		}
		*registered = true
		slog.Info("registered with the server: approve this agent on the server's Agents page after checking its fingerprint matches",
			"server", cfg.Server, "fingerprint", a.Fingerprint())
	}

	cctx, cancel := call()
	resp, err := client.Poll(cctx, &agentpb.PollRequest{})
	cancel()
	if status.Code(err) == codes.NotFound {
		*registered = false // the registration was forgotten; start over
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch resp.GetStatus() {
	case agentpb.RegistrationStatus_REGISTRATION_STATUS_APPROVED:
		if err := a.finishEnrollment(resp.GetAgentId(), resp.GetCertDer(), resp.GetCaDer()); err != nil {
			return false, fmt.Errorf("install approved certificate: %w", err)
		}
		slog.Info("approved and enrolled", "id", a.ID())
		return true, nil
	case agentpb.RegistrationStatus_REGISTRATION_STATUS_REJECTED:
		if !*rejectedLogged {
			slog.Warn("the server rejected this agent; remove the registration on its Agents page to register again")
			*rejectedLogged = true
		}
	default:
		*rejectedLogged = false
	}
	return false, nil
}

// finishEnrollment verifies and installs the certificate the server issued,
// and builds the Kopia engine whose host name is the agent ID.
func (a *Agent) finishEnrollment(id string, certDER, caDER []byte) error {
	ca, err := x509.ParseCertificate(caDER)
	if err != nil || !ca.IsCA {
		return errors.New("invalid CA certificate")
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return errors.New("invalid agent certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if id == "" {
		return errors.New("the server sent no agent ID")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   pki.AgentDNSName(id),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("agent certificate does not verify: %w", err)
	}
	if cert.Subject.CommonName != id || !a.key.PublicKey.Equal(cert.PublicKey) {
		return errors.New("agent certificate is not for this agent")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.enrolled != nil {
		return nil
	}
	eng, err := engine.New(a.engineDir(), id)
	if err != nil {
		return fmt.Errorf("create engine: %w", err)
	}
	if err := eng.SetCache(a.cache); err != nil {
		return fmt.Errorf("create engine: %w", err)
	}
	// agent.crt last: its presence is what marks the agent enrolled.
	if err := pki.WriteCert(a.path(fileCA), caDER); err != nil {
		return fmt.Errorf("save CA: %w", err)
	}
	if err := pki.WriteCert(a.path(fileCert), certDER); err != nil {
		return fmt.Errorf("save certificate: %w", err)
	}
	a.engine = eng
	a.enrolled = &identity{id: id, cert: pki.TLSCert(certDER, a.key), ca: ca, cas: pool}
	return nil
}

// RenewCertificate installs a newly issued certificate for this agent, and the
// CA certificate that comes with it, in place of the ones it has. The server
// sends them before the old ones expire. Nothing about who the agent is can
// change this way: the certificate must be for this agent's ID and key, and
// the CA must carry the same key as the CA the agent already trusts, which is
// the only thing that could have signed either.
func (a *Agent) RenewCertificate(_ context.Context, req *agentpb.RenewCertificateRequest) (*agentpb.RenewCertificateResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.enrolled
	if cur == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent is not enrolled")
	}
	ca, caDER := cur.ca, []byte(nil)
	if der := req.GetCaDer(); len(der) > 0 {
		next, err := x509.ParseCertificate(der)
		if err != nil || !next.IsCA {
			return nil, status.Error(codes.InvalidArgument, "invalid CA certificate")
		}
		pub, ok := next.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
		if !ok || !pub.Equal(cur.ca.PublicKey) {
			return nil, status.Error(codes.PermissionDenied, "the CA certificate is not for the CA this agent trusts")
		}
		if !next.Equal(cur.ca) {
			ca, caDER = next, der
		}
	}
	cert, err := x509.ParseCertificate(req.GetCertDer())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid agent certificate")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   pki.AgentDNSName(cur.id),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "agent certificate does not verify: %v", err)
	}
	if cert.Subject.CommonName != cur.id || !a.key.PublicKey.Equal(cert.PublicKey) {
		return nil, status.Error(codes.PermissionDenied, "agent certificate is not for this agent")
	}
	// The CA first: the certificate the agent still has verifies against the
	// renewed CA too (same key), so stopping between the two writes is harmless.
	if caDER != nil {
		if err := pki.WriteCert(a.path(fileCA), caDER); err != nil {
			return nil, status.Errorf(codes.Internal, "save CA: %v", err)
		}
	}
	if err := pki.WriteCert(a.path(fileCert), cert.Raw); err != nil {
		return nil, status.Errorf(codes.Internal, "save certificate: %v", err)
	}
	a.enrolled = &identity{id: cur.id, cert: pki.TLSCert(cert.Raw, a.key), ca: ca, cas: pool}
	slog.Info("certificate renewed", "id", cur.id, "valid_until", cert.NotAfter.Format(time.RFC3339), "ca_renewed", caDER != nil)
	return &agentpb.RenewCertificateResponse{}, nil
}
