package agent

import (
	"context"
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
	conn, err := grpc.NewClient(cfg.Server, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{a.bootstrap},
		InsecureSkipVerify: true, // the pin below replaces chain verification
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
	defer conn.Close()
	client := agentpb.NewRegistryClient(conn)

	call := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 15*time.Second) }
	if !*registered {
		cctx, cancel := call()
		_, err := client.Register(cctx, &agentpb.RegisterRequest{
			Secret: tok.Secret, Hostname: a.hostname, Version: version.Version, ListenPort: int32(cfg.ListenPort),
		})
		cancel()
		if err != nil {
			return false, err
		}
		*registered = true
		slog.Info("registered with the server: approve this agent on the server's Agents page", "server", cfg.Server)
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
	// agent.crt last: its presence is what marks the agent enrolled.
	if err := pki.WriteCert(a.path(fileCA), caDER); err != nil {
		return fmt.Errorf("save CA: %w", err)
	}
	if err := pki.WriteCert(a.path(fileCert), certDER); err != nil {
		return fmt.Errorf("save certificate: %w", err)
	}
	a.engine = eng
	a.enrolled = &identity{id: id, cert: pki.TLSCert(certDER, a.key), cas: pool}
	return nil
}
