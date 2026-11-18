// Package netcheck implements the `check` subcommand both binaries expose for
// troubleshooting connections (the image has no ping, dig or nc). It resolves
// a host:port, then opens a TCP connection to each address it resolved to.
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// lookup resolves a name; a variable so a test can fail it without depending
// on the resolver of the machine it runs on.
var lookup = net.DefaultResolver.LookupHost

// say writes one line of the report. A failed write to the terminal has
// nowhere better to be reported, so it is dropped.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// Run checks target (host:port) and writes what it found to w. It returns an
// error unless at least one address accepted a connection.
func Run(ctx context.Context, w io.Writer, target string, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return fmt.Errorf("address must be host:port: %w", err)
	}
	var addrs []string
	if ip := net.ParseIP(host); ip != nil {
		say(w, "address   %s (an IP, no DNS lookup)\n", ip)
		addrs = []string{ip.String()}
	} else {
		lctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		start := time.Now()
		addrs, err = lookup(lctx, host)
		if err != nil {
			say(w, "dns       FAILED  %s: %v\n", host, err)
			return errors.New("name did not resolve")
		}
		say(w, "dns       ok      %s -> %v (%s)\n", host, addrs, time.Since(start).Round(time.Millisecond))
	}
	reached := 0
	for _, a := range addrs {
		start := time.Now()
		d := net.Dialer{Timeout: timeout}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a, port))
		if err != nil {
			say(w, "tcp       FAILED  %s: %v\n", net.JoinHostPort(a, port), err)
			continue
		}
		tidy.CloseWarn("connection being checked", conn)
		reached++
		say(w, "tcp       ok      %s (%s)\n", net.JoinHostPort(a, port), time.Since(start).Round(time.Millisecond))
	}
	if reached == 0 {
		return errors.New("no address accepted a connection")
	}
	return nil
}

// Main runs the subcommand with the arguments after `check` and exits the
// process: 0 when the target is reachable, 1 when not, 2 on bad usage.
func Main(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: check <host:port>   (resolves the name, then opens a TCP connection)")
		os.Exit(2)
	}
	if err := Run(context.Background(), os.Stdout, args[0], 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "unreachable:", err)
		os.Exit(1)
	}
	fmt.Println("reachable")
	os.Exit(0)
}
