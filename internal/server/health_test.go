package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/agent"
	"github.com/voidgrid/voidgrid-backup/internal/health"
)

func TestHealth(t *testing.T) {
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	// /healthz is exempt from both setup and sign-in, so the plain default
	// client is enough here.
	get(t, http.DefaultClient, srv.URL+"/healthz", "ok")
	c.Catalog.Close()
	if resp, err := http.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a closed catalog: %v %v", resp, err)
	}

	a, addr := startAgent(t, t.TempDir())
	hs := httptest.NewServer(a.HealthHandler(addr))
	defer hs.Close()
	resp, err := http.Get(hs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var st agent.HealthStatus
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !st.Healthy || st.Enrolled || st.Docker != "disabled" {
		t.Fatalf("agent health: %d %+v", resp.StatusCode, st)
	}
	if st := a.Health(context.Background(), "127.0.0.1:1"); st.Healthy || st.GRPC == "ok" {
		t.Fatalf("agent reported healthy with no gRPC listener: %+v", st)
	}

	for listen, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9444":   "http://127.0.0.1:9444/healthz",
		"127.0.0.1:9444": "http://127.0.0.1:9444/healthz",
		"10.0.0.5:80":    "http://10.0.0.5:80/healthz",
	} {
		if got, err := health.LocalURL(listen, "/healthz"); err != nil || got != want {
			t.Errorf("LocalURL(%q) = %q, %v", listen, got, err)
		}
	}
}
