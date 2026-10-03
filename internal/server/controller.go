// Package server is the controller side: it enrolls and checks agents and
// serves the web UI and API.
package server

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

var ErrNameTaken = errors.New("an agent with that name already exists")

type Controller struct {
	Catalog    *catalog.Catalog
	CA         *pki.CA
	ClientCert tls.Certificate
	Timeout    time.Duration // per agent check/enroll call; 0 means 10s
	// LogBuf holds the server's own recent log lines for the Logs page; nil
	// turns capture off.
	LogBuf *logbuf.Buffer
	// SessionKey signs session cookies; set once at startup
	// (webauth.LoadOrCreateSessionKey) and never changed after.
	SessionKey []byte
	// FlagOIDCConfig is the OIDC config given as flags/env at startup, if
	// any. When set (Enabled()), it always wins over whatever is saved in
	// the catalog -- see ReloadAuth.
	FlagOIDCConfig webauth.Config

	authMu sync.RWMutex
	auth   *webauth.Authenticator

	inflight running
	usage    usageCache
	browse   browseCache
	gate     repoGate
	// snapLocks holds a *sync.Mutex per job ID; see snapLock.
	snapLocks sync.Map
}

// Auth is the server's own login, always present (see package webauth:
// signing in is never optional). It's swappable so that saving new OIDC
// settings through the setup wizard or the authentication settings page
// takes effect immediately -- see ReloadAuth -- without restarting the
// process; every caller should fetch it fresh via this method rather than
// holding onto a copy.
func (c *Controller) Auth() *webauth.Authenticator {
	c.authMu.RLock()
	defer c.authMu.RUnlock()
	return c.auth
}

func (c *Controller) setAuth(a *webauth.Authenticator) {
	c.authMu.Lock()
	c.auth = a
	c.authMu.Unlock()
}

// ReloadAuth rebuilds Auth from the current OIDC settings -- FlagOIDCConfig
// if given, otherwise whatever's saved in the catalog (possibly nothing,
// meaning recovery-code-only) -- and installs it immediately. Called once
// at startup and again whenever the setup wizard or the authentication
// settings page saves a new OIDC configuration.
func (c *Controller) ReloadAuth(ctx context.Context) error {
	cfg := c.FlagOIDCConfig
	if !cfg.Enabled() {
		stored, err := c.OIDCConfig(ctx)
		if err != nil {
			return fmt.Errorf("load OIDC settings: %w", err)
		}
		cfg = stored
	}
	a, err := webauth.New(ctx, cfg, c.SessionKey)
	if err != nil {
		// A process that won't boot at all is a worse lockout than OIDC
		// being temporarily unavailable -- fall back to recovery-code-only
		// sign-in (SetOIDCConfig already validates before ever saving a
		// config here, so this is for the case that changed after: the
		// issuer moved, a certificate expired, the network is down).
		slog.Error("OIDC unavailable at startup; falling back to recovery-code-only sign-in", "err", err)
		a, err = webauth.New(ctx, webauth.Config{}, c.SessionKey)
		if err != nil {
			return err
		}
	}
	c.setAuth(a)
	return nil
}

func (c *Controller) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 10 * time.Second
}

// Enroll pairs the agent at address using the code from its log, records it,
// and checks it over mTLS before returning.
func (c *Controller) Enroll(ctx context.Context, name, address, code string) (catalog.Agent, error) {
	name, address = strings.TrimSpace(name), strings.TrimSpace(address)
	if name == "" {
		return catalog.Agent{}, errors.New("name is required")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return catalog.Agent{}, fmt.Errorf("address must be host:port: %w", err)
	}
	if _, err := c.Catalog.AgentByName(ctx, name); err == nil {
		return catalog.Agent{}, ErrNameTaken
	} else if !errors.Is(err, catalog.ErrNotFound) {
		return catalog.Agent{}, err
	}
	parsed, err := enroll.Parse(code)
	if err != nil {
		return catalog.Agent{}, err
	}

	// The agent's bootstrap cert is self-signed, so chain verification is
	// replaced by the pin in the enrollment code.
	var (
		pinMu  sync.Mutex
		pinned crypto.PublicKey
	)
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("agent presented no certificate")
			}
			leaf := cs.PeerCertificates[0]
			if !parsed.Matches(leaf.Raw) {
				return errors.New("agent certificate does not match the enrollment code")
			}
			pinMu.Lock()
			pinned = leaf.PublicKey
			pinMu.Unlock()
			return nil
		},
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		return catalog.Agent{}, err
	}
	defer conn.Close()
	client := agentpb.NewAgentClient(conn)

	id, err := newAgentID()
	if err != nil {
		return catalog.Agent{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	resp, err := client.Enroll(callCtx, &agentpb.EnrollRequest{Secret: parsed.Secret, AgentId: id})
	if err != nil {
		return catalog.Agent{}, fmt.Errorf("enroll: %w", err)
	}
	pinMu.Lock()
	pub := pinned
	pinMu.Unlock()
	if pub == nil {
		return catalog.Agent{}, errors.New("enroll: agent certificate was not pinned")
	}
	certDER, err := c.CA.SignAgentCSR(resp.GetCsrDer(), id, pub)
	if err != nil {
		return catalog.Agent{}, fmt.Errorf("enroll: %w", err)
	}
	if _, err := client.CompleteEnrollment(callCtx, &agentpb.CompleteEnrollmentRequest{
		Secret: parsed.Secret, CertDer: certDER, CaDer: c.CA.Cert.Raw,
	}); err != nil {
		return catalog.Agent{}, fmt.Errorf("complete enrollment: %w", err)
	}

	a := catalog.Agent{
		ID:              id,
		Name:            name,
		Address:         address,
		Hostname:        resp.GetHostname(),
		Version:         resp.GetVersion(),
		CertFingerprint: hex.EncodeToString(pki.Fingerprint(certDER)),
		EnrolledAt:      time.Now(),
	}
	if err := c.Catalog.AddAgent(ctx, a); err != nil {
		return catalog.Agent{}, fmt.Errorf("agent %s is enrolled but could not be recorded (reset the agent's data directory to enroll it again): %w", id, err)
	}
	if err := c.Check(ctx, a); err != nil {
		return a, fmt.Errorf("enrolled, but the mTLS check failed: %w", err)
	}
	return c.Catalog.Agent(ctx, id)
}

// dialAgent opens an mTLS connection to an enrolled agent.
func (c *Controller) dialAgent(a catalog.Agent) (*grpc.ClientConn, error) {
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{c.ClientCert},
		RootCAs:      c.CA.Pool(),
		ServerName:   pki.AgentDNSName(a.ID),
	}
	return grpc.NewClient(a.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
}

// Check pings an enrolled agent over mTLS and records the result.
func (c *Controller) Check(ctx context.Context, a catalog.Agent) error {
	conn, err := c.dialAgent(a)
	if err == nil {
		defer conn.Close()
		callCtx, cancel := context.WithTimeout(ctx, c.timeout())
		defer cancel()
		var resp *agentpb.PingResponse
		if resp, err = agentpb.NewAgentClient(conn).Ping(callCtx, &agentpb.PingRequest{}); err == nil {
			if resp.GetAgentId() != a.ID {
				err = fmt.Errorf("agent reports ID %q, expected %q", resp.GetAgentId(), a.ID)
			} else {
				return c.Catalog.RecordSeen(ctx, a.ID, time.Now(), resp.GetHostname(), resp.GetVersion(), agentWarnings(resp))
			}
		}
	}
	if rerr := c.Catalog.RecordFailure(ctx, a.ID, err.Error()); rerr != nil {
		slog.Warn("record agent failure", "agent", a.ID, "err", rerr)
	}
	return err
}

// agentWarnings turns an agent's self-report into what the Agents page shows.
func agentWarnings(p *agentpb.PingResponse) []string {
	missing := p.GetMissingCapabilities()
	if len(missing) == 0 {
		return nil
	}
	out := []string{fmt.Sprintf("runs as uid %d without full permissions: backups skip files it can't read and restores can't set ownership. Run the agent as root with the capabilities in the README.", p.GetUid())}
	return append(out, missing...)
}

// Poll checks every agent now and then every interval until ctx ends.
func (c *Controller) Poll(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		agents, err := c.Catalog.ListAgents(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("list agents", "err", err)
		}
		for _, a := range agents {
			if err := c.Check(ctx, a); err != nil && ctx.Err() == nil {
				slog.Warn("agent check failed", "agent", a.Name, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func newAgentID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
