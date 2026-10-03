// Command voidgrid-backup-agent runs on each host and serves the agent gRPC
// API to the server. `voidgrid-backup-agent healthcheck` probes a running agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/voidgrid/voidgrid-backup/internal/agent"
	"github.com/voidgrid/voidgrid-backup/internal/enroll"
	"github.com/voidgrid/voidgrid-backup/internal/envflag"
	"github.com/voidgrid/voidgrid-backup/internal/health"
	"github.com/voidgrid/voidgrid-backup/internal/logbuf"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/version"
	"github.com/voidgrid/voidgrid-backup/internal/virt"
)

type config struct {
	listen, healthListen, data              string
	server, token                           string
	dockerSocket, libvirtSocket, libvirtURI string
	logs                                    *logbuf.Buffer
}

func main() {
	listen := envflag.String("listen", "VB_LISTEN", ":9443", "gRPC address the server dials")
	serverAddr := envflag.String("server", "VB_SERVER", "", "host:port of the server's agent registration listener; needed until the agent is enrolled")
	token := envflag.String("token", "VB_TOKEN", "", "registration token from the server; needed until the agent is enrolled")
	healthListen := envflag.String("health-listen", "VB_HEALTH_LISTEN", "127.0.0.1:9444", "HTTP address for /healthz; keep it on loopback")
	data := envflag.String("data", "VB_DATA", "/data", "directory for the agent's key, certificates and Kopia cache")
	dockerSocket := envflag.String("docker", "VB_DOCKER_SOCKET", "/var/run/docker.sock", "Docker socket for stack backups; empty disables them")
	libvirtSocket := envflag.String("libvirt-socket", "VB_LIBVIRT_SOCKET", "/var/run/libvirt/libvirt-sock", "libvirt socket for VM backups; empty disables them")
	libvirtURI := envflag.String("libvirt-uri", "VB_LIBVIRT_URI", "qemu:///system", "libvirt connection URI")

	args := os.Args[1:]
	healthcheck := len(args) > 0 && args[0] == "healthcheck"
	if healthcheck {
		args = args[1:]
	}
	flag.CommandLine.Parse(args)
	c := config{listen: *listen, healthListen: *healthListen, data: *data, server: *serverAddr, token: *token,
		dockerSocket: *dockerSocket, libvirtSocket: *libvirtSocket, libvirtURI: *libvirtURI}
	if healthcheck {
		url, err := health.LocalURL(c.healthListen, "/healthz")
		if err != nil {
			slog.Error("healthcheck", "err", err)
			os.Exit(1)
		}
		health.Probe(url)
	}
	// Keep the recent log in memory too, for the server's Logs page. What
	// goes to stderr (docker logs) is unchanged.
	c.logs = logbuf.New(2000)
	slog.SetDefault(slog.New(c.logs.Handler(logbuf.NewStderrHandler(os.Stderr, slog.LevelInfo))))
	if err := run(c); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(c config) error {
	if c.dockerSocket != "" {
		if _, err := os.Stat(c.dockerSocket); err != nil {
			slog.Warn("docker socket not available; stack backups disabled", "socket", c.dockerSocket, "err", err)
			c.dockerSocket = ""
		}
	}
	a, err := agent.New(c.data, c.dockerSocket)
	if err != nil {
		return err
	}
	if a.ID() == "" {
		if c.server == "" || c.token == "" {
			return errors.New("this agent is not enrolled yet: set VB_SERVER (host:port of the server's agent registration listener) and VB_TOKEN (from the server's Agents page, or `voidgrid-backup-server token`)")
		}
		if _, err := enroll.Parse(c.token); err != nil {
			return fmt.Errorf("VB_TOKEN: %w", err)
		}
	}
	a.LogBuf = c.logs
	if c.libvirtSocket != "" {
		if _, err := os.Stat(c.libvirtSocket); err != nil {
			slog.Warn("libvirt socket not available; VM backups disabled", "socket", c.libvirtSocket, "err", err)
		} else {
			a.SetHypervisor(&virt.Libvirt{Socket: c.libvirtSocket, URI: c.libvirtURI})
		}
	}
	lis, err := net.Listen("tcp", c.listen)
	if err != nil {
		return err
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(a.TLSConfig())), grpc.UnaryInterceptor(a.Interceptor))
	agentpb.RegisterAgentServer(s, a)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if id := a.ID(); id != "" {
		slog.Info("enrolled", "id", id)
	} else {
		slog.Info("not enrolled: registering with the server", "server", c.server, "fingerprint", a.Fingerprint())
		go func() {
			err := a.Register(ctx, agent.RegisterConfig{Server: c.server, Token: c.token, ListenPort: lis.Addr().(*net.TCPAddr).Port})
			if err != nil && ctx.Err() == nil {
				slog.Error("registration stopped", "err", err)
			}
		}()
	}

	if c.healthListen != "" {
		hs := &http.Server{Addr: c.healthListen, Handler: a.HealthHandler(lis.Addr().String()), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("health endpoint", "addr", c.healthListen, "err", err)
			}
		}()
		defer hs.Close()
	}

	go func() {
		<-ctx.Done()
		s.GracefulStop()
	}()
	slog.Info("voidgrid-backup-agent listening", "addr", c.listen, "health", c.healthListen, "version", version.Version)
	return s.Serve(lis)
}
