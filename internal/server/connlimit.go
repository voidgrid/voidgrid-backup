package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// checkLocationFree refuses a repository location that another repository
// already uses: the same SFTP host, port, user and path, the same S3 bucket and
// prefix, or the same directory. Two repositories in one place would write over
// each other's blobs. It also refuses an S3 prefix that lists another
// repository's objects (see repocfg.Config.SharesS3Listing). except is the repository being edited, if any.
func (c *Controller) checkLocationFree(ctx context.Context, cfg repocfg.Config, except string) error {
	id := cfg.Identity()
	if id == "" {
		return nil
	}
	repos, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		if r.ID == except {
			continue
		}
		if r.Config.Identity() == id {
			return fmt.Errorf("the repository %q already uses %s; every repository needs its own path", r.Name, cfg.Location())
		}
		if cfg.SharesS3Listing(r.Config) {
			return fmt.Errorf("the S3 prefix %q starts the same way as the prefix %q of the repository %q in the same bucket, so each would list the other's objects as its own; end the prefix with a / or pick one that does not begin like the other",
				cfg.S3.Prefix, r.Config.S3.Prefix, r.Name)
		}
	}
	return nil
}

// hetznerBoxLimit is what Hetzner documents for a Storage Box: up to 10
// simultaneous connections per account
// (https://docs.hetzner.com/robot/storage-box/general).
const hetznerBoxLimit = 10

const sftpCapKey = "sftp_conn_cap"

// The ways the limit on simultaneous SFTP connections can be set.
const (
	// CapAuto limits a Hetzner Storage Box to hetznerBoxLimit and leaves every
	// other host alone. It is the default.
	CapAuto = "auto"
	// CapOff never limits.
	CapOff = "off"
	// CapFixed limits every SFTP host to the number saved with it.
	CapFixed = "fixed"
)

// SFTPCap is the saved setting for the limit on simultaneous connections to
// one SFTP host.
type SFTPCap struct {
	Mode string `json:"mode"`
	N    int    `json:"n,omitempty"` // for CapFixed
}

// SFTPCap returns the saved setting: CapAuto unless it was changed.
func (c *Controller) SFTPCap(ctx context.Context) SFTPCap {
	v, err := c.Catalog.GetSetting(ctx, sftpCapKey)
	v = strings.TrimSpace(v)
	switch {
	case err != nil || v == "" || v == CapAuto:
		return SFTPCap{Mode: CapAuto}
	case v == CapOff:
		return SFTPCap{Mode: CapOff}
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return SFTPCap{Mode: CapFixed, N: n}
	}
	return SFTPCap{Mode: CapAuto}
}

// SetSFTPCap saves how the SFTP connection limit is set.
func (c *Controller) SetSFTPCap(ctx context.Context, mode string, n int) error {
	var v string
	switch mode {
	case CapAuto, CapOff:
		v = mode
	case CapFixed:
		if n < 1 || n > 1000 {
			return errors.New("the number of connections must be between 1 and 1000")
		}
		v = strconv.Itoa(n)
	default:
		return errors.New(`the mode must be "auto", "fixed" or "off"`)
	}
	return c.Catalog.SetSetting(ctx, sftpCapKey, v)
}

// Describe is the setting as the Repositories page words it.
func (s SFTPCap) Describe() string {
	switch s.Mode {
	case CapOff:
		return "off"
	case CapFixed:
		return fmt.Sprintf("%d connections per host", s.N)
	}
	return fmt.Sprintf("automatic (%d on a Hetzner Storage Box, none elsewhere)", hetznerBoxLimit)
}

// isHetznerBox says whether host is a Hetzner Storage Box.
func isHetznerBox(host string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".your-storagebox.de")
}

// connLimit is the account the repository's SFTP connections count against and
// how many may be open at once; limit 0 means no limit (not SFTP, or off). The
// account is the host and port, not the user: Hetzner counts per box, and a
// box's sub-accounts are not documented to have connections of their own.
func (c *Controller) connLimit(ctx context.Context, cfg repocfg.Config) (key string, limit int) {
	if cfg.SFTP == nil {
		return "", 0
	}
	key = fmt.Sprintf("%s:%d", strings.ToLower(cfg.SFTP.Host), cfg.SFTP.Port)
	switch s := c.SFTPCap(ctx); s.Mode {
	case CapOff:
		return key, 0
	case CapFixed:
		return key, s.N
	}
	if isHetznerBox(cfg.SFTP.Host) {
		return key, hetznerBoxLimit
	}
	return key, 0
}

// connSlots counts the operations using each SFTP host's connections, and
// holds back further ones while a host is at its limit. Each operation that
// opens a repository holds one connection for as long as it runs.
type connSlots struct {
	mu   sync.Mutex
	used map[string]int
	wake chan struct{} // closed when a slot is given back
}

// acquire waits until fewer than limit operations use key, or ctx ends. The
// returned func gives the slot back. A limit of 0 never waits.
func (s *connSlots) acquire(ctx context.Context, key string, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	for {
		s.mu.Lock()
		if s.used == nil {
			s.used = map[string]int{}
		}
		if s.used[key] < limit {
			s.used[key]++
			s.mu.Unlock()
			return func() { s.release(key) }, nil
		}
		if s.wake == nil {
			s.wake = make(chan struct{})
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *connSlots) release(key string) {
	s.mu.Lock()
	s.used[key]--
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
	s.mu.Unlock()
}

// storageSlot takes a connection to the repository's SFTP host for one
// operation, waiting while the host is at its limit. It is a no-op for other
// storage and when the limit is off. Call the returned func when the operation
// has finished with the repository.
func (c *Controller) storageSlot(ctx context.Context, repo catalog.Repository) (func(), error) {
	key, limit := c.connLimit(ctx, repo.Config)
	start := time.Now()
	release, err := c.conns.acquire(ctx, key, limit)
	if err != nil {
		return nil, fmt.Errorf("waiting for a free connection to %s: %w", repo.Config.SFTP.Host, err)
	}
	if w := time.Since(start); w > 2*time.Second {
		slog.Info("waited for a free storage connection", "repo", repo.Name, "host", key, "limit", limit, "waited", w.Round(time.Second))
	}
	return release, nil
}
