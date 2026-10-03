package server

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

const (
	settingRegistrationSecret = "registration_secret"
	// maxUndecided caps pending and rejected registrations so a leaked token
	// cannot fill the catalog.
	maxUndecided = 50
)

// RegistrationSecret returns the secret half of the registration token,
// creating it on first use.
func (c *Controller) RegistrationSecret(ctx context.Context) ([]byte, error) {
	c.regMu.Lock()
	defer c.regMu.Unlock()
	return c.registrationSecretLocked(ctx)
}

func (c *Controller) registrationSecretLocked(ctx context.Context) ([]byte, error) {
	stored, err := c.Catalog.GetSetting(ctx, settingRegistrationSecret)
	if err != nil {
		return nil, err
	}
	if stored != "" {
		if s, err := hex.DecodeString(stored); err == nil && len(s) == enroll.SecretLen {
			return s, nil
		}
	}
	s, err := enroll.NewSecret()
	if err != nil {
		return nil, err
	}
	return s, c.Catalog.SetSetting(ctx, settingRegistrationSecret, hex.EncodeToString(s))
}

// RegistrationToken returns the token an operator puts in an agent's
// configuration: the secret plus a pin of the registration listener's
// certificate.
func (c *Controller) RegistrationToken(ctx context.Context) (string, error) {
	secret, err := c.RegistrationSecret(ctx)
	if err != nil {
		return "", err
	}
	if len(c.RegistryCert.Certificate) == 0 {
		return "", errors.New("registration certificate is not loaded")
	}
	return enroll.Format(secret, c.RegistryCert.Certificate[0]), nil
}

// RotateRegistrationToken replaces the secret. Agents that registered but were
// not decided yet are dropped; agents that are already approved are
// unaffected, since they authenticate with their certificate from then on.
func (c *Controller) RotateRegistrationToken(ctx context.Context) (string, error) {
	c.regMu.Lock()
	secret, err := enroll.NewSecret()
	if err == nil {
		err = c.Catalog.SetSetting(ctx, settingRegistrationSecret, hex.EncodeToString(secret))
	}
	if err == nil {
		err = c.Catalog.DeleteUndecidedRegistrations(ctx)
	}
	c.regMu.Unlock()
	if err != nil {
		return "", err
	}
	return c.RegistrationToken(ctx)
}

// ServeRegistry serves the agent registration listener on lis until ctx ends.
func (c *Controller) ServeRegistry(ctx context.Context, lis net.Listener) error {
	creds := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{c.RegistryCert},
		// Agents present a self-signed certificate. It is not verified against
		// a CA; the handshake proving possession of its key is what matters,
		// and the operator's approval is what grants anything.
		ClientAuth: tls.RequireAnyClientCert,
		NextProtos: []string{"h2"},
	})
	s := grpc.NewServer(grpc.Creds(creds))
	agentpb.RegisterRegistryServer(s, &registry{c: c})
	go func() {
		<-ctx.Done()
		s.GracefulStop()
	}()
	return s.Serve(lis)
}

type registry struct {
	agentpb.UnimplementedRegistryServer
	c *Controller
}

// peerCert returns the agent's self-signed certificate from the handshake.
func peerCert(ctx context.Context) (*x509.Certificate, *peer.Peer, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, nil, status.Error(codes.Unauthenticated, "no peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return nil, nil, status.Error(codes.Unauthenticated, "client certificate required")
	}
	return info.State.PeerCertificates[0], p, nil
}

func (r *registry) Register(ctx context.Context, req *agentpb.RegisterRequest) (*agentpb.RegisterResponse, error) {
	cert, p, err := peerCert(ctx)
	if err != nil {
		return nil, err
	}
	secret, err := r.c.RegistrationSecret(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load registration secret: %v", err)
	}
	if subtle.ConstantTimeCompare(req.GetSecret(), secret) != 1 {
		slog.Warn("agent registration refused: wrong token", "from", p.Addr.String())
		return nil, status.Error(codes.PermissionDenied, "wrong registration token")
	}
	port := int(req.GetListenPort())
	if port < 1 || port > 65535 {
		return nil, status.Error(codes.InvalidArgument, "invalid listen port")
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "peer address: %v", err)
	}
	fp := hex.EncodeToString(pki.Fingerprint(cert.Raw))
	if _, err := r.c.Catalog.RegistrationByFingerprint(ctx, fp); errors.Is(err, catalog.ErrNotFound) {
		list, err := r.c.Catalog.ListRegistrations(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list registrations: %v", err)
		}
		if len(list) >= maxUndecided {
			return nil, status.Error(codes.ResourceExhausted, "too many undecided registrations; approve, reject or forget some")
		}
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "look up registration: %v", err)
	}
	id, err := newAgentID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "new ID: %v", err)
	}
	reg, err := r.c.Catalog.RegisterAgent(ctx, catalog.Registration{
		ID:          id,
		Fingerprint: fp,
		AgentCert:   cert.Raw,
		Hostname:    cleanText(req.GetHostname(), 100),
		Address:     net.JoinHostPort(host, strconv.Itoa(port)),
		Version:     cleanText(req.GetVersion(), 50),
		CreatedAt:   time.Now(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record registration: %v", err)
	}
	return &agentpb.RegisterResponse{RegistrationId: reg.ID}, nil
}

func (r *registry) Poll(ctx context.Context, _ *agentpb.PollRequest) (*agentpb.PollResponse, error) {
	cert, _, err := peerCert(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := r.c.Catalog.RegistrationByFingerprint(ctx, hex.EncodeToString(pki.Fingerprint(cert.Raw)))
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "not registered")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "look up registration: %v", err)
	}
	if err := r.c.Catalog.TouchRegistration(ctx, reg.ID, time.Now()); err != nil {
		slog.Warn("record registration poll", "registration", reg.ID, "err", err)
	}
	switch reg.Status {
	case catalog.RegApproved:
		return &agentpb.PollResponse{
			Status:  agentpb.RegistrationStatus_REGISTRATION_STATUS_APPROVED,
			AgentId: reg.AgentID, CertDer: reg.IssuedCert, CaDer: r.c.CA.Cert.Raw,
		}, nil
	case catalog.RegRejected:
		return &agentpb.PollResponse{Status: agentpb.RegistrationStatus_REGISTRATION_STATUS_REJECTED}, nil
	}
	return &agentpb.PollResponse{Status: agentpb.RegistrationStatus_REGISTRATION_STATUS_PENDING}, nil
}

// ApproveRegistration accepts a pending registration. With replaceID empty it
// adds a new agent called name at address; otherwise the registered agent
// takes over the existing agent replaceID (same ID, so its jobs and snapshots
// carry over) at address, keeping that agent's name.
func (c *Controller) ApproveRegistration(ctx context.Context, regID, name, address, replaceID string) (catalog.Agent, error) {
	name, address = strings.TrimSpace(name), strings.TrimSpace(address)
	if _, _, err := net.SplitHostPort(address); err != nil {
		return catalog.Agent{}, fmt.Errorf("address must be host:port: %w", err)
	}
	reg, err := c.Catalog.RegistrationByID(ctx, regID)
	if err != nil {
		return catalog.Agent{}, err
	}
	if reg.Status != catalog.RegPending {
		return catalog.Agent{}, fmt.Errorf("registration is %s, not pending", reg.Status)
	}
	agentCert, err := x509.ParseCertificate(reg.AgentCert)
	if err != nil {
		return catalog.Agent{}, fmt.Errorf("agent certificate: %w", err)
	}

	agentID := reg.ID
	if replaceID != "" {
		agentID = replaceID
		if _, err := c.Catalog.Agent(ctx, replaceID); err != nil {
			return catalog.Agent{}, err
		}
	} else {
		if name == "" {
			return catalog.Agent{}, errors.New("name is required")
		}
		if _, err := c.Catalog.AgentByName(ctx, name); err == nil {
			return catalog.Agent{}, ErrNameTaken
		} else if !errors.Is(err, catalog.ErrNotFound) {
			return catalog.Agent{}, err
		}
	}
	certDER, err := c.CA.SignAgentKey(agentID, agentCert.PublicKey)
	if err != nil {
		return catalog.Agent{}, err
	}
	fingerprint := hex.EncodeToString(pki.Fingerprint(certDER))
	if replaceID != "" {
		err = c.Catalog.ReplaceAgent(ctx, agentID, address, reg.Hostname, reg.Version, fingerprint)
	} else {
		err = c.Catalog.AddAgent(ctx, catalog.Agent{
			ID: agentID, Name: name, Address: address, Hostname: reg.Hostname, Version: reg.Version,
			CertFingerprint: fingerprint, EnrolledAt: time.Now(),
		})
	}
	if err != nil {
		return catalog.Agent{}, err
	}
	if err := c.Catalog.ApproveRegistration(ctx, reg.ID, agentID, certDER); err != nil {
		return catalog.Agent{}, fmt.Errorf("agent %s was added but its registration could not be marked approved: %w", agentID, err)
	}
	return c.Catalog.Agent(ctx, agentID)
}

// RejectRegistration refuses a pending registration.
func (c *Controller) RejectRegistration(ctx context.Context, regID string) error {
	return c.Catalog.RejectRegistration(ctx, regID)
}

// ForgetRegistration deletes a registration (typically a rejected one) so the
// agent can register again.
func (c *Controller) ForgetRegistration(ctx context.Context, regID string) error {
	return c.Catalog.DeleteRegistration(ctx, regID)
}

// RenameAgent changes an agent's display name; its ID, and so its snapshots,
// are untouched.
func (c *Controller) RenameAgent(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name is required")
	}
	if other, err := c.Catalog.AgentByName(ctx, name); err == nil && other.ID != id {
		return ErrNameTaken
	} else if err != nil && !errors.Is(err, catalog.ErrNotFound) {
		return err
	}
	return c.Catalog.RenameAgent(ctx, id, name)
}

// cleanText trims s, drops control characters and cuts it to max runes. It is
// applied to what an agent reports about itself before it is stored and shown.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}
