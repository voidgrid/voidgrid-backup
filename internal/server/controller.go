// Package server is the controller side: it enrolls and checks agents and
// serves the web UI and API.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

var ErrNameTaken = errors.New("an agent with that name already exists")

type Controller struct {
	Catalog    *catalog.Catalog
	CA         *pki.CA
	ClientCert tls.Certificate
	// RegistryCert is what the agent registration listener presents; the
	// registration token pins it (see pki.LoadOrCreateServerTLSCert).
	RegistryCert tls.Certificate
	Timeout      time.Duration // per agent check/enroll call; 0 means 10s
	// LogBuf holds the server's own recent log lines for the Logs page; nil
	// turns capture off.
	LogBuf *logbuf.Buffer
	// Engine lets the server open repositories itself, for the ones it
	// maintains (see maintby.go). Nil means the server maintains nothing.
	Engine *engine.Engine
	// SessionKey signs session cookies; set once at startup
	// (webauth.LoadOrCreateSessionKey) and never changed after.
	SessionKey []byte
	// FlagOIDCConfig is the OIDC config given as flags/env at startup, if
	// any. When set (Enabled()), it always wins over whatever is saved in
	// the catalog -- see ReloadAuth.
	FlagOIDCConfig webauth.Config

	authMu sync.RWMutex
	auth   *webauth.Authenticator

	// recoveryMu serialises reading, using and replacing the recovery codes: a
	// code is checked and marked used in two steps, and two requests with the
	// same code must not both get past the first.
	recoveryMu sync.Mutex
	// regMu serialises creating and rotating the registration secret.
	regMu sync.Mutex

	inflight running
	progress progressBook
	usage    usageCache
	browse   browseCache
	gate     repoGate
	cancels  runCancels // the Cancel button for runs in progress
	conns    connSlots  // simultaneous connections per SFTP host
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
	a.SetSignOuts(signOuts{c.Catalog})
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

// dialAgent opens an mTLS connection to an enrolled agent. Besides the CA
// chain and the agent's ID, the certificate must be the exact one issued at
// approval: after an agent is replaced, the old host's certificate (same ID,
// still within its validity) is refused.
func (c *Controller) dialAgent(a catalog.Agent) (*grpc.ClientConn, error) {
	want := a.CertFingerprint
	// While a renewal is under way the agent may present either certificate:
	// the one on record, or the new one it was just sent (see renewAgentCert).
	next, err := c.Catalog.GetSetting(context.Background(), agentNextCertKey(a.ID))
	if err != nil {
		return nil, fmt.Errorf("look up the agent's certificate: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{c.ClientCert},
		RootCAs:      c.CA.Pool(),
		ServerName:   pki.AgentDNSName(a.ID),
		// VerifyConnection runs after chain verification, on every
		// connection including resumed ones.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("agent presented no certificate")
			}
			got := hex.EncodeToString(pki.Fingerprint(cs.PeerCertificates[0].Raw))
			issued := want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
			renewed := next != "" && subtle.ConstantTimeCompare([]byte(got), []byte(next)) == 1
			if !issued && !renewed {
				return errors.New("agent certificate is not the one issued when this agent was approved (replaced or re-registered host?)")
			}
			return nil
		},
	}
	return grpc.NewClient(a.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20)))
}

// Check pings an enrolled agent over mTLS and records the result.
func (c *Controller) Check(ctx context.Context, a catalog.Agent) error {
	conn, err := c.dialAgent(a)
	if err == nil {
		defer tidy.CloseWarn("connection to the agent", conn)
		callCtx, cancel := context.WithTimeout(ctx, c.timeout())
		defer cancel()
		var resp *agentpb.PingResponse
		var p peer.Peer
		if resp, err = agentpb.NewAgentClient(conn).Ping(callCtx, &agentpb.PingRequest{}, grpc.Peer(&p)); err == nil {
			if resp.GetAgentId() != a.ID {
				err = fmt.Errorf("agent reports ID %q, expected %q", resp.GetAgentId(), a.ID)
			} else {
				c.renewAgentCert(ctx, a, conn, &p)
				// The agent has its certificate now; its approved
				// registration row has done its job.
				if derr := c.Catalog.DeleteApprovedRegistrations(ctx, a.ID); derr != nil {
					slog.Warn("clear approved registration", "agent", a.ID, "err", derr)
				}
				return c.Catalog.RecordSeen(ctx, a.ID, time.Now(), resp.GetHostname(), resp.GetVersion(), agentWarnings(resp))
			}
		}
	}
	if rerr := c.Catalog.RecordFailure(ctx, a.ID, err.Error()); rerr != nil {
		slog.Warn("record agent failure", "agent", a.ID, "err", rerr)
	}
	return err
}

// agentRenewBefore is how long before an agent's certificate expires the server
// replaces it. A variable so that tests can make a new certificate count as due.
var agentRenewBefore = pki.AgentRenewBefore

func agentNextCertKey(agentID string) string { return "agent_cert_next:" + agentID }

// renewAgentCert keeps the certificate of an agent the server has just reached
// from running out. When it is within agentRenewBefore of its end, or was
// issued under an earlier CA certificate than the server has now, the server
// signs a new one for the same key and sends it with the CA certificate.
//
// The agent may switch to the new certificate and the server then fail to
// record that, or the reverse, so the new fingerprint is first saved as one
// the agent may also present (dialAgent accepts both) and only becomes the one
// on record once the agent is seen using it. A failure is logged and tried
// again at the next check; an agent too old to renew keeps its certificate.
func (c *Controller) renewAgentCert(ctx context.Context, a catalog.Agent, conn *grpc.ClientConn, p *peer.Peer) {
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return
	}
	leaf := info.State.PeerCertificates[0]
	got := hex.EncodeToString(pki.Fingerprint(leaf.Raw))
	if got != a.CertFingerprint {
		// It presents the certificate it was sent: that one is on record now.
		if err := c.Catalog.SetAgentCertFingerprint(ctx, a.ID, got); err != nil {
			slog.Warn("record the agent's renewed certificate", "agent", a.Name, "err", err)
			return
		}
		if err := c.Catalog.SetSetting(ctx, agentNextCertKey(a.ID), ""); err != nil {
			slog.Warn("record the agent's renewed certificate", "agent", a.Name, "err", err)
		}
		slog.Info("agent certificate renewed", "agent", a.Name, "valid_until", leaf.NotAfter.Format(time.RFC3339))
	}
	if time.Until(leaf.NotAfter) > agentRenewBefore && !leaf.NotBefore.Before(c.CA.Cert.NotBefore) {
		return
	}
	certDER, err := c.CA.SignAgentKey(a.ID, leaf.PublicKey)
	if err != nil {
		slog.Warn("renew the agent's certificate", "agent", a.Name, "err", err)
		return
	}
	if err := c.Catalog.SetSetting(ctx, agentNextCertKey(a.ID), hex.EncodeToString(pki.Fingerprint(certDER))); err != nil {
		slog.Warn("renew the agent's certificate", "agent", a.Name, "err", err)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	_, err = agentpb.NewAgentClient(conn).RenewCertificate(callCtx, &agentpb.RenewCertificateRequest{CertDer: certDER, CaDer: c.CA.Cert.Raw})
	switch {
	case status.Code(err) == codes.Unimplemented:
		slog.Warn("the agent's certificate is due for renewal, but the agent is too old to take a new one: update it",
			"agent", a.Name, "valid_until", leaf.NotAfter.Format(time.RFC3339))
	case err != nil:
		slog.Warn("renew the agent's certificate", "agent", a.Name, "err", err)
	}
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

// signOuts keeps, in the catalog's settings, when each identity last signed
// out: what makes signing out end a session on the server and not only in the
// browser that asked (see webauth.SignOuts).
type signOuts struct{ cat *catalog.Catalog }

func signedOutKey(identity string) string { return "signed_out:" + identity }

func (s signOuts) SignedOutAt(ctx context.Context, identity string) (time.Time, error) {
	v, err := s.cat.GetSetting(ctx, signedOutKey(identity))
	if err != nil || v == "" {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored sign-out time of %s: %w", identity, err)
	}
	return time.Unix(0, n), nil
}

func (s signOuts) SignOut(ctx context.Context, identity string, at time.Time) error {
	return s.cat.SetSetting(ctx, signedOutKey(identity), strconv.FormatInt(at.UnixNano(), 10))
}
