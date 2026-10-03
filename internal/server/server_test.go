package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/agent"
	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/pki"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/webauth"
)

// startAgent serves an agent from dir on a loopback port.
func startAgent(t *testing.T, dir string) (*agent.Agent, string) {
	t.Helper()
	return startAgentWithDocker(t, dir, "")
}

func startAgentWithDocker(t *testing.T, dir, dockerSocket string) (*agent.Agent, string) {
	t.Helper()
	a, err := agent.New(dir, dockerSocket)
	if err != nil {
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
	return a, lis.Addr().String()
}

func newController(t *testing.T) *Controller {
	t.Helper()
	dir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	ca, err := pki.LoadOrCreateCA(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := pki.LoadOrCreateClientCert(filepath.Join(dir, "pki"), ca)
	if err != nil {
		t.Fatal(err)
	}
	// Signing in is mandatory (see internal/webauth), so every Controller
	// needs an Authenticator to satisfy NewHandler -- a bare one, no OIDC,
	// same as a real install that hasn't configured a provider. Setup
	// itself is left incomplete here, matching a fresh install; tests that
	// need past it use authedClient, and http_setup_test.go's own
	// newAuthController covers the setup/sign-in flow itself.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	auth, err := webauth.New(context.Background(), webauth.Config{}, key)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := pki.LoadOrCreateServerTLSCert(filepath.Join(dir, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{Catalog: cat, CA: ca, ClientCert: client, RegistryCert: registry, SessionKey: key, auth: auth}
}

// authedClient completes setup for c if it hasn't been already, and returns
// an http.Client whose cookie jar carries a valid session for srv -- for
// tests that need to reach protected routes without exercising the setup
// wizard or sign-in flow themselves (see http_setup_test.go for that).
func authedClient(t *testing.T, c *Controller, srv *httptest.Server) *http.Client {
	t.Helper()
	ctx := context.Background()
	if done, err := c.SetupComplete(ctx); err != nil {
		t.Fatal(err)
	} else if !done {
		if _, err := c.GenerateRecoveryCodes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c.Auth().IssueSession(rec, httptest.NewRequest("GET", "/", nil), "test")
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, rec.Result().Cookies())
	return &http.Client{Jar: jar}
}

// anonPing calls Ping without a client certificate.
func anonPing(t *testing.T, addr string) error {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(
		&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = agentpb.NewAgentClient(conn).Ping(context.Background(), &agentpb.PingRequest{})
	return err
}

// enrollTestAgent registers a with c over a loopback registration listener,
// approves it as name at addr, and waits until the server has reached it over
// mTLS, the way an operator's registration and approval end up.
func enrollTestAgent(t *testing.T, c *Controller, a *agent.Agent, name, addr string) (catalog.Agent, error) {
	t.Helper()
	return approveTestAgent(t, c, a, name, addr, "")
}

// approveTestAgent is enrollTestAgent with a choice of replacing an existing
// agent (replaceID) instead of adding a new one.
func approveTestAgent(t *testing.T, c *Controller, a *agent.Agent, name, addr, replaceID string) (catalog.Agent, error) {
	t.Helper()
	ctx := context.Background()
	regAddr := startRegistry(t, c)
	token, err := c.RegistrationToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go a.Register(rctx, agent.RegisterConfig{Server: regAddr, Token: token, ListenPort: port, PollEvery: 20 * time.Millisecond})

	var reg catalog.Registration
	waitFor(t, "the agent's registration", func() bool {
		list, err := c.Catalog.ListRegistrations(ctx)
		if err == nil && len(list) == 1 {
			reg = list[0]
			return true
		}
		return false
	})
	ag, err := c.ApproveRegistration(ctx, reg.ID, name, addr, replaceID)
	if err != nil {
		return catalog.Agent{}, err
	}
	waitFor(t, "the agent to collect its certificate", func() bool { return a.ID() != "" })
	if err := c.Check(ctx, ag); err != nil {
		return ag, err
	}
	return c.Catalog.Agent(ctx, ag.ID)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRegisterPingAndRestart(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	a, addr := startAgent(t, agentDir)
	if a.ID() != "" {
		t.Fatal("fresh agent claims an identity")
	}
	if err := anonPing(t, addr); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Ping before enrollment: %v", err)
	}

	c := newController(t)
	got, err := enrollTestAgent(t, c, a, "box", addr)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeen.IsZero() || got.LastError != "" || got.ID != a.ID() || got.Name != "box" {
		t.Fatalf("enrolled agent record: %+v (agent ID %q)", got, a.ID())
	}
	// Without the server's client cert the agent refuses the handshake.
	if err := anonPing(t, addr); err == nil {
		t.Fatal("enrolled agent answered a client without a certificate")
	}
	// The approved registration row is gone once the server reached the agent.
	if list, _ := c.Catalog.ListRegistrations(ctx); len(list) != 0 {
		t.Fatalf("registrations left: %+v", list)
	}

	// A restart from the same directory keeps the identity.
	a2, addr2 := startAgent(t, agentDir)
	if a2.ID() != got.ID {
		t.Fatalf("restarted agent: id %q, want %q", a2.ID(), got.ID)
	}
	got.Address = addr2
	if err := c.Check(ctx, got); err != nil {
		t.Fatalf("Check after restart: %v", err)
	}
}

func TestHTTP(t *testing.T) {
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()

	// Complete setup (the wizard itself is http_setup_test.go's job) but
	// don't sign in yet: nothing should work without a session either.
	if _, err := c.GenerateRecoveryCodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp, err := http.Get(srv.URL + "/api/agents"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/agents = %v %v, want 401", resp, err)
	}
	client := authedClient(t, c, srv)

	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}

	form := url.Values{"name": {"box"}, "address": {"127.0.0.1:1"}, "code": {"not-a-code"}}
	req, _ := http.NewRequest("POST", srv.URL+"/agents", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d, want 403 even while signed in", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", srv.URL+"/agents/registrations/nope/approve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("approving an unknown registration = %d, want 400", resp.StatusCode)
	}

	resp, err = client.Get(srv.URL + "/api/agents")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /api/agents = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// restoreOK accepts a successful restore, or, when the tests don't run as
// root, one whose only warning is that ownership could not be restored.
func restoreOK(r catalog.Run) bool {
	if r.Status == catalog.RunSuccess {
		return true
	}
	return os.Geteuid() != 0 && r.Status == catalog.RunPartial &&
		strings.HasPrefix(r.Error, "file ownership was not restored") && !strings.Contains(r.Error, "\n")
}
