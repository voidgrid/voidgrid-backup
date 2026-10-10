package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// TestRepository connects to the repository's storage and opens it with its
// password. It never creates anything at the location. The warnings are
// findings that do not make the repository unusable.
func (e *Engine) TestRepository(ctx context.Context, r Repo) (warnings []string, err error) {
	if err := r.Config.Validate(); err != nil {
		return nil, err
	}
	if err := noRepositoryAt(ctx, r.Config); err != nil {
		return nil, err
	}
	dir := filepath.Join(e.dir, r.ID+"-"+r.Config.Fingerprint())
	_, statErr := os.Stat(dir)
	fresh := errors.Is(statErr, fs.ErrNotExist)

	rep, err := e.open(ctx, r)
	if err != nil {
		// Do not leave a half-made local directory for a location that
		// does not hold a repository.
		if _, cfgErr := os.Stat(filepath.Join(dir, "repository.config")); fresh && errors.Is(cfgErr, fs.ErrNotExist) {
			_ = os.RemoveAll(dir)
		}
		return nil, err
	}
	if err := rep.Close(ctx); err != nil {
		return nil, fmt.Errorf("close repository: %w", err)
	}
	if fsc := r.Config.Filesystem; fsc != nil {
		if w := mountWarning(fsc.Path); w != "" {
			warnings = append(warnings, w)
		}
	}
	return warnings, nil
}

// networkFS are the filesystem types treated as a network mount: SMB/CIFS
// shares and NFS.
var networkFS = map[string]bool{"cifs": true, "smb3": true, "nfs": true, "nfs4": true}

// mountWarning returns a warning when p is not on a network mount, "" when it
// is or when the mount table cannot be read. A share that drops leaves an
// empty directory on the local disk, which a backup would then fill.
func mountWarning(p string) string {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	fstype, point := mountOf(string(data), p)
	if fstype == "" || networkFS[fstype] {
		return ""
	}
	return fmt.Sprintf("%s is on a local %s filesystem (mounted at %s), not a network mount. If this is meant to be an SMB or NFS share, check that it is mounted.", p, fstype, point)
}

// mountOf returns the filesystem type and mount point of the longest mount
// point in a /proc/self/mountinfo table that contains p.
func mountOf(mountinfo, p string) (fstype, point string) {
	for _, line := range strings.Split(mountinfo, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields := strings.Fields(before)
		typ := strings.Fields(after)
		if len(fields) < 5 || len(typ) < 1 {
			continue
		}
		mp := unescapeMountinfo(fields[4])
		if !within(mp, p) || len(mp) < len(point) {
			continue
		}
		fstype, point = typ[0], mp
	}
	return fstype, point
}

// unescapeMountinfo decodes the \040-style octal escapes of a mountinfo path.
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// within reports whether p is dir or lies under it.
func within(dir, p string) bool {
	dir, p = path.Clean(dir), path.Clean(p)
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// MoveRepository renames the repository's directory from the location in
// from to the one in to. Only SFTP and filesystem locations can be moved, and
// both must be of the same kind on the same server. It refuses when no
// repository is found at the source or anything exists at the destination, so
// nothing is overwritten. The caller verifies the result and moves back on
// failure.
func (e *Engine) MoveRepository(ctx context.Context, from, to repocfg.Config) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if from.Kind != to.Kind {
		return errors.New("a repository cannot be moved to a different kind of storage")
	}
	if from.SFTP != nil && from.SFTP.KnownHosts == "" {
		return errors.New("no pinned host key for this SFTP repository; refusing to connect without one")
	}
	oldPath, newPath, err := movePaths(from, to)
	if err != nil {
		return err
	}
	if err := requireRepositoryAt(ctx, from); err != nil {
		return err
	}
	switch from.Kind {
	case repocfg.KindFilesystem:
		return moveFilesystem(oldPath, newPath)
	case repocfg.KindSFTP:
		if from.SFTP.Host != to.SFTP.Host || from.SFTP.Port != to.SFTP.Port || from.SFTP.User != to.SFTP.User {
			return errors.New("the host, port and user of a repository cannot change in a move")
		}
		return moveSFTP(ctx, *to.SFTP, oldPath, newPath)
	}
	return fmt.Errorf("a %s repository cannot be moved", from.Kind)
}

// movePaths returns the cleaned old and new directory and refuses a move onto
// itself or into itself.
func movePaths(from, to repocfg.Config) (oldPath, newPath string, err error) {
	switch from.Kind {
	case repocfg.KindFilesystem:
		oldPath, newPath = from.Filesystem.Path, to.Filesystem.Path
	case repocfg.KindSFTP:
		oldPath, newPath = from.SFTP.Path, to.SFTP.Path
	default:
		return "", "", fmt.Errorf("a %s repository cannot be moved", from.Kind)
	}
	oldPath, newPath = path.Clean(oldPath), path.Clean(newPath)
	switch {
	case oldPath == newPath:
		return "", "", errors.New("the new path is the same as the current one")
	case within(oldPath, newPath):
		return "", "", errors.New("the new path is inside the current one")
	}
	return oldPath, newPath, nil
}

// requireRepositoryAt fails unless Kopia's format blob exists at the location,
// so a wrong path moves nothing.
func requireRepositoryAt(ctx context.Context, c repocfg.Config) error {
	if err := noRepositoryAt(ctx, c); err != nil {
		return err
	}
	st, err := storage(ctx, c, false)
	if err != nil {
		return fmt.Errorf("open the current location: %w", err)
	}
	defer st.Close(ctx) //nolint:errcheck // read-only probe
	if _, err := st.GetMetadata(ctx, format.KopiaRepositoryBlobID); errors.Is(err, blob.ErrBlobNotFound) {
		return errors.New("no repository was found at the current location; nothing was moved")
	} else if err != nil {
		return fmt.Errorf("look for the repository: %w", err)
	}
	return nil
}

// errNoRepository is returned by noRepositoryAt when the location holds no
// Kopia repository.
var errNoRepository = errors.New("no repository was found at this location; nothing was changed")

// noRepositoryAt fails with errNoRepository when c is a filesystem or SFTP
// location without Kopia's format blob. Opening Kopia's file or SFTP storage on
// an empty directory writes its sharding file into it, so a wrong path is
// rejected, with a read-only look, before any storage is opened. Other kinds
// return nil.
func noRepositoryAt(ctx context.Context, c repocfg.Config) error {
	switch {
	case c.Filesystem != nil:
		found, err := filepath.Glob(filepath.Join(c.Filesystem.Path, format.KopiaRepositoryBlobID+".*"))
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return errNoRepository
		}
	case c.SFTP != nil:
		cli, closeFn, err := dialSFTP(ctx, *c.SFTP)
		if err != nil {
			return err
		}
		defer closeFn()
		infos, err := cli.ReadDir(c.SFTP.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return errNoRepository
		} else if err != nil {
			return fmt.Errorf("look for the repository: %w", err)
		}
		for _, fi := range infos {
			if strings.HasPrefix(fi.Name(), format.KopiaRepositoryBlobID+".") {
				return nil
			}
		}
		return errNoRepository
	}
	return nil
}

func moveFilesystem(oldPath, newPath string) error {
	if _, err := os.Lstat(newPath); err == nil {
		return fmt.Errorf("%s already exists; nothing was moved", newPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		return fmt.Errorf("create the parent of the new path: %w", err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return errors.New("the new path is on a different filesystem; moving across filesystems is not supported, move the directory yourself and edit without moving the data")
		}
		return fmt.Errorf("rename %s to %s: %w", oldPath, newPath, err)
	}
	return nil
}

func moveSFTP(ctx context.Context, s repocfg.SFTP, oldPath, newPath string) error {
	cli, closeFn, err := dialSFTP(ctx, s)
	if err != nil {
		return err
	}
	defer closeFn()

	if _, err := cli.Lstat(newPath); err == nil {
		return fmt.Errorf("%s already exists; nothing was moved", newPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look at the new path: %w", err)
	}
	if dir := path.Dir(newPath); dir != "." && dir != "/" {
		if err := cli.MkdirAll(dir); err != nil {
			return fmt.Errorf("create the parent of the new path: %w", err)
		}
	}
	if err := cli.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("rename %s to %s on the server: %w", oldPath, newPath, err)
	}
	return nil
}

// dialSFTP opens an SFTP session the way Kopia's SFTP storage does: the
// password when there is one, otherwise the key file, and the pinned host key.
func dialSFTP(ctx context.Context, s repocfg.SFTP) (*pkgsftp.Client, func(), error) {
	hostKey, err := hostKeyCallback(s.KnownHosts)
	if err != nil {
		return nil, nil, err
	}
	var auth []ssh.AuthMethod
	if s.Password != "" {
		auth = append(auth, ssh.Password(s.Password))
	} else {
		if !filepath.IsAbs(s.KeyFile) {
			return nil, nil, errors.New("key file path must be absolute")
		}
		pem, err := os.ReadFile(s.KeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read private key file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, nil, fmt.Errorf("parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, &ssh.ClientConfig{
		User: s.User, Auth: auth, HostKeyCallback: hostKey, Timeout: 30 * time.Second,
	})
	if err != nil {
		conn.Close() //nolint:errcheck,gosec // already failing
		return nil, nil, fmt.Errorf("ssh to %s: %w", addr, err)
	}
	cli, err := pkgsftp.NewClient(ssh.NewClient(sc, chans, reqs))
	if err != nil {
		sc.Close() //nolint:errcheck,gosec // already failing
		return nil, nil, fmt.Errorf("start sftp on %s: %w", addr, err)
	}
	return cli, func() { _ = cli.Close(); _ = sc.Close() }, nil
}

// hostKeyCallback checks the server against known_hosts content. knownhosts
// only reads files, so the content goes through a temporary one.
func hostKeyCallback(data string) (ssh.HostKeyCallback, error) {
	f, err := os.CreateTemp("", "known_hosts-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // the content is not secret
	if _, err := f.WriteString(data); err != nil {
		f.Close() //nolint:errcheck,gosec // already failing
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	cb, err := knownhosts.New(f.Name())
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	return cb, nil
}
