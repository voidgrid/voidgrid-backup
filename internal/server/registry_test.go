package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// startRegistry serves c's registration listener on loopback and returns its
// address.
func startRegistry(t *testing.T, c *Controller) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.ServeRegistry(ctx, lis); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return lis.Addr().String()
}

// testAgent is the agent side of registration: a self-signed identity.
type testAgent struct {
	key  *ecdsa.PrivateKey
	cert tls.Certificate
	reg  agentpb.RegistryClient
}

// newTestAgent connects to addr the way an agent does: it pins the server's
// certificate with the token and presents its own self-signed certificate.
func newTestAgent(t *testing.T, addr, token string) *testAgent {
	t.Helper()
	tok, err := enroll.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pki.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	der, err := pki.SelfSigned(key, "voidgrid-backup-agent bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	cert := pki.TLSCert(der, key)
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !tok.Matches(cs.PeerCertificates[0].Raw) {
				return errors.New("server certificate does not match the token")
			}
			return nil
		},
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testAgent{key: key, cert: cert, reg: agentpb.NewRegistryClient(conn)}
}

func (a *testAgent) register(t *testing.T, secret []byte) (*agentpb.RegisterResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.reg.Register(ctx, &agentpb.RegisterRequest{Secret: secret, Hostname: "box", Version: "v-test", ListenPort: 9443})
}

func (a *testAgent) poll(t *testing.T) (*agentpb.PollResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.reg.Poll(ctx, &agentpb.PollRequest{})
}

func tokenParts(t *testing.T, c *Controller) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	token, err := c.RegistrationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := enroll.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	return token, parsed.Secret
}

func TestRegisterRejectsWrongSecret(t *testing.T) {
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	a := newTestAgent(t, addr, token)

	wrong := append([]byte(nil), secret...)
	wrong[0] ^= 0xff
	if _, err := a.register(t, wrong); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong secret: %v", err)
	}
	if list, _ := c.Catalog.ListRegistrations(context.Background()); len(list) != 0 {
		t.Fatalf("a refused registration was recorded: %+v", list)
	}
}

func TestAgentRejectsWrongServer(t *testing.T) {
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	// A token for some other server's certificate must not connect.
	other, err := pki.LoadOrCreateServerTLSCert(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, addr, enroll.Format(secret, other.Certificate[0]))
	if _, err := a.register(t, secret); err == nil {
		t.Fatal("agent connected to a server its token does not pin")
	}
	_ = token
}

func TestRegisterApproveCollect(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	a := newTestAgent(t, addr, token)

	resp, err := a.register(t, secret)
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.register(t, secret)
	if err != nil || again.GetRegistrationId() != resp.GetRegistrationId() {
		t.Fatalf("registering twice: %v %v", again, err)
	}
	if p, err := a.poll(t); err != nil || p.GetStatus() != agentpb.RegistrationStatus_REGISTRATION_STATUS_PENDING {
		t.Fatalf("poll before decision: %v %v", p, err)
	}

	list, _ := c.Catalog.ListRegistrations(ctx)
	if len(list) != 1 || list[0].Address != "127.0.0.1:9443" || list[0].Hostname != "box" {
		t.Fatalf("pending list: %+v", list)
	}

	if _, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "", list[0].Address, ""); err == nil {
		t.Fatal("approved without a name")
	}
	if _, err := c.Catalog.AgentByName(ctx, "box"); err == nil {
		t.Fatal("failed approval created an agent")
	}
	ag, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "box", list[0].Address, "")
	if err != nil {
		t.Fatal(err)
	}
	if ag.ID != resp.GetRegistrationId() || ag.Name != "box" || ag.Address != "127.0.0.1:9443" {
		t.Fatalf("approved agent: %+v", ag)
	}
	if _, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "again", list[0].Address, ""); err == nil {
		t.Fatal("approved the same registration twice")
	}

	p, err := a.poll(t)
	if err != nil || p.GetStatus() != agentpb.RegistrationStatus_REGISTRATION_STATUS_APPROVED || p.GetAgentId() != ag.ID {
		t.Fatalf("poll after approval: %v %v", p, err)
	}
	ca, err := x509.ParseCertificate(p.GetCaDer())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(p.GetCertDer())
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: pki.AgentDNSName(ag.ID), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("issued certificate does not verify: %v", err)
	}
	if !a.key.PublicKey.Equal(cert.PublicKey) {
		t.Fatal("issued certificate is not for the agent's key")
	}

	// The approved row goes once the server has reached the agent; Check
	// does that, so simulate its cleanup directly here.
	if err := c.Catalog.DeleteApprovedRegistrations(ctx, ag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.poll(t); status.Code(err) != codes.NotFound {
		t.Fatalf("poll after cleanup: %v", err)
	}
}

func TestApproveNameTaken(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	if err := c.Catalog.AddAgent(ctx, catalog.Agent{ID: "a0", Name: "box", Address: "h:1", CertFingerprint: "x", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, addr, token)
	resp, err := a.register(t, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "box", "h:2", ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestRejectAndForget(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	a := newTestAgent(t, addr, token)
	resp, err := a.register(t, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RejectRegistration(ctx, resp.GetRegistrationId()); err != nil {
		t.Fatal(err)
	}
	if p, err := a.poll(t); err != nil || p.GetStatus() != agentpb.RegistrationStatus_REGISTRATION_STATUS_REJECTED {
		t.Fatalf("poll after reject: %v %v", p, err)
	}
	// Registering again does not resurrect it.
	if again, err := a.register(t, secret); err != nil || again.GetRegistrationId() != resp.GetRegistrationId() {
		t.Fatalf("re-register after reject: %v %v", again, err)
	}
	if p, _ := a.poll(t); p.GetStatus() != agentpb.RegistrationStatus_REGISTRATION_STATUS_REJECTED {
		t.Fatalf("rejected agent became %v", p.GetStatus())
	}
	if _, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "box", "h:1", ""); err == nil {
		t.Fatal("approved a rejected registration")
	}
	if err := c.ForgetRegistration(ctx, resp.GetRegistrationId()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.poll(t); status.Code(err) != codes.NotFound {
		t.Fatalf("poll after forget: %v", err)
	}
}

func TestRotateToken(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	if again, err := c.RegistrationToken(ctx); err != nil || again != token {
		t.Fatalf("token should be stable: %q vs %q (%v)", again, token, err)
	}
	a := newTestAgent(t, addr, token)
	if _, err := a.register(t, secret); err != nil {
		t.Fatal(err)
	}

	next, err := c.RotateRegistrationToken(ctx)
	if err != nil || next == token {
		t.Fatalf("rotate: %q %v", next, err)
	}
	if list, _ := c.Catalog.ListRegistrations(ctx); len(list) != 0 {
		t.Fatalf("pending registrations survived rotation: %+v", list)
	}
	if _, err := a.register(t, secret); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("old secret after rotation: %v", err)
	}
	parsed, _ := enroll.Parse(next)
	if _, err := a.register(t, parsed.Secret); err != nil {
		t.Fatalf("new secret: %v", err)
	}
}

func TestReplaceKeepsAgentID(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	addr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	if err := c.Catalog.AddAgent(ctx, catalog.Agent{ID: "a-old", Name: "box", Address: "old:1", CertFingerprint: "old", EnrolledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	a := newTestAgent(t, addr, token)
	resp, err := a.register(t, secret)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "ignored", "new:2", "a-old")
	if err != nil {
		t.Fatal(err)
	}
	if ag.ID != "a-old" || ag.Name != "box" || ag.Address != "new:2" || ag.CertFingerprint == "old" {
		t.Fatalf("replaced agent: %+v", ag)
	}
	p, err := a.poll(t)
	if err != nil || p.GetAgentId() != "a-old" {
		t.Fatalf("poll after replace: %v %v", p, err)
	}
	cert, _ := x509.ParseCertificate(p.GetCertDer())
	if cert.Subject.CommonName != "a-old" || !a.key.PublicKey.Equal(cert.PublicKey) {
		t.Fatalf("certificate for the replaced agent: CN=%q", cert.Subject.CommonName)
	}
	if _, err := c.ApproveRegistration(ctx, resp.GetRegistrationId(), "", "x:1", "missing"); err == nil {
		t.Fatal("replaced an agent that does not exist")
	}
}

func TestRenameAgent(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	for _, a := range []catalog.Agent{
		{ID: "a1", Name: "one", Address: "h:1", CertFingerprint: "x", EnrolledAt: time.Now()},
		{ID: "a2", Name: "two", Address: "h:2", CertFingerprint: "y", EnrolledAt: time.Now()},
	} {
		if err := c.Catalog.AddAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RenameAgent(ctx, "a1", " renamed "); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Catalog.Agent(ctx, "a1"); got.Name != "renamed" || got.ID != "a1" {
		t.Fatalf("after rename: %+v", got)
	}
	if err := c.RenameAgent(ctx, "a1", "two"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	if err := c.RenameAgent(ctx, "a1", "renamed"); err != nil {
		t.Fatalf("renaming to the same name: %v", err)
	}
	if err := c.RenameAgent(ctx, "a1", "  "); err == nil {
		t.Fatal("blank name accepted")
	}
}

func TestCleanText(t *testing.T) {
	if got := cleanText("  ho\x00st\n  ", 10); got != "host" {
		t.Fatalf("control characters kept: %q", got)
	}
	if got := cleanText("abcdef", 3); got != "abc" {
		t.Fatalf("not truncated: %q", got)
	}
}

func TestAgentsPageApproval(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	regAddr := startRegistry(t, c)
	token, secret := tokenParts(t, c)
	a := newTestAgent(t, regAddr, token)
	if _, err := a.register(t, secret); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	page := get(t, client, srv.URL+"/", "Waiting for approval")
	for _, want := range []string{token, "Approve", "127.0.0.1:9443", "v-test"} {
		if !strings.Contains(page, want) {
			t.Fatalf("Agents page is missing %q:\n%s", want, page)
		}
	}

	list, _ := c.Catalog.ListRegistrations(ctx)
	if len(list) != 1 {
		t.Fatalf("registrations: %+v", list)
	}
	post(t, client, srv.URL+"/agents/registrations/"+list[0].ID+"/approve",
		url.Values{"name": {"web box"}, "address": {"127.0.0.1:9443"}})
	ag, err := c.Catalog.AgentByName(ctx, "web box")
	if err != nil || ag.ID != list[0].ID {
		t.Fatalf("approving through the page: %+v %v", ag, err)
	}
	get(t, client, srv.URL+"/", "web box")

	post(t, client, srv.URL+"/agents/"+ag.ID+"/rename", url.Values{"name": {"renamed box"}})
	if got, _ := c.Catalog.Agent(ctx, ag.ID); got.Name != "renamed box" {
		t.Fatalf("rename through the page: %+v", got)
	}

	post(t, client, srv.URL+"/agents/token/rotate", url.Values{})
	if next, _ := c.RegistrationToken(ctx); next == token {
		t.Fatal("rotate through the page kept the token")
	}
	if strings.Contains(get(t, client, srv.URL+"/", "Registration token"), token) {
		t.Fatal("the old token is still shown after rotating")
	}
}

func TestReplacedAgentOldCertRefused(t *testing.T) {
	ctx := context.Background()
	c := newController(t)
	oldAgent, oldAddr := startAgent(t, t.TempDir())
	old, err := enrollTestAgent(t, c, oldAgent, "box", oldAddr)
	if err != nil {
		t.Fatal(err)
	}

	// The host is rebuilt: a new agent (new key) is approved as replacing it.
	newAgent, newAddr := startAgent(t, t.TempDir())
	replaced, err := approveTestAgent(t, c, newAgent, "", newAddr, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.ID != old.ID || newAgent.ID() != old.ID || replaced.Name != "box" || replaced.CertFingerprint == old.CertFingerprint {
		t.Fatalf("replacement: %+v (new agent ID %q)", replaced, newAgent.ID())
	}

	// The old host still holds a CA-signed certificate for the same ID. The
	// server must refuse it now that the record points at the new one.
	stale := replaced
	stale.Address = oldAddr
	if err := c.Check(ctx, stale); err == nil || !strings.Contains(err.Error(), "not the one issued") {
		t.Fatalf("old certificate after replacement: %v", err)
	}
	stale.Address = newAddr
	if err := c.Check(ctx, stale); err != nil {
		t.Fatalf("new agent after replacement: %v", err)
	}
}
