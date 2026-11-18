package netcheck

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestRunReachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	for _, target := range []string{l.Addr().String(), "localhost:" + portOf(t, l)} {
		var out bytes.Buffer
		if err := Run(context.Background(), &out, target, time.Second); err != nil {
			t.Fatalf("%s: %v\n%s", target, err, out.String())
		}
		if !strings.Contains(out.String(), "tcp       ok") {
			t.Fatalf("%s: no ok line:\n%s", target, out.String())
		}
	}
}

func TestRunClosedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var out bytes.Buffer
	if err := Run(context.Background(), &out, addr, time.Second); err == nil {
		t.Fatalf("reported a closed port as reachable:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAILED") {
		t.Fatalf("no FAILED line:\n%s", out.String())
	}
}

func TestRunBadInput(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), &out, "no-port", time.Second); err == nil {
		t.Fatal("accepted an address without a port")
	}
	lookup = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
	defer func() { lookup = net.DefaultResolver.LookupHost }()
	if err := Run(context.Background(), &out, "agent:1", time.Second); err == nil {
		t.Fatal("reported an unresolvable name as reachable")
	}
	if !strings.Contains(out.String(), "dns       FAILED") {
		t.Fatalf("no dns FAILED line:\n%s", out.String())
	}
}

func portOf(t *testing.T, l net.Listener) string {
	t.Helper()
	_, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
