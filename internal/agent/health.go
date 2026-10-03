package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"time"
)

// HealthStatus is what /healthz reports.
type HealthStatus struct {
	Healthy  bool   `json:"healthy"`
	GRPC     string `json:"grpc"`     // "ok" or the dial error
	Enrolled bool   `json:"enrolled"` // false: waiting for the server to approve this agent
	Docker   string `json:"docker"`   // "ok", "disabled" or the error
	Libvirt  string `json:"libvirt"`  // "ok", "disabled" or the error
	DataDir  string `json:"data_dir"` // "ok" or why it can't be written
}

// HealthHandler serves /healthz. Only a gRPC listener that doesn't accept
// connections or an unwritable data directory make the agent unhealthy;
// Docker and libvirt are optional, so their state is reported, not judged.
func (a *Agent) HealthHandler(grpcAddr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		st := a.Health(ctx, grpcAddr)
		w.Header().Set("Content-Type", "application/json")
		if !st.Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(st) //nolint:errcheck // status already sent; a failed write means the client went away
	})
	return mux
}

func (a *Agent) Health(ctx context.Context, grpcAddr string) HealthStatus {
	st := HealthStatus{GRPC: "ok", Enrolled: a.ID() != "", Docker: "disabled", Libvirt: "disabled", DataDir: "ok"}
	var d net.Dialer
	if conn, err := d.DialContext(ctx, "tcp", loopback(grpcAddr)); err != nil {
		st.GRPC = err.Error()
	} else {
		conn.Close()
	}
	if f, err := os.CreateTemp(a.dir, ".health-*"); err != nil {
		st.DataDir = err.Error()
	} else {
		f.Close()           //nolint:errcheck // writability probe; the file is deleted on the next line
		os.Remove(f.Name()) //nolint:errcheck // best-effort cleanup of the probe file
	}
	if a.docker != nil {
		st.Docker = "ok"
		if _, err := a.docker.Containers(ctx); err != nil {
			st.Docker = err.Error()
		}
	}
	if a.hv != nil {
		st.Libvirt = "ok"
		if _, err := a.hv.Domains(ctx); err != nil {
			st.Libvirt = err.Error()
		}
	}
	st.Healthy = st.GRPC == "ok" && st.DataDir == "ok"
	return st
}

func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
