// Package repocfg describes where a backup repository lives. The server
// stores it and sends it to agents; agents turn it into Kopia storage.
package repocfg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

const (
	KindSFTP       = "sftp"
	KindS3         = "s3"
	KindFilesystem = "filesystem"
)

type Config struct {
	Kind       string      `json:"kind"`
	SFTP       *SFTP       `json:"sftp,omitempty"`
	S3         *S3         `json:"s3,omitempty"`
	Filesystem *Filesystem `json:"filesystem,omitempty"`
	// ECC enables Reed-Solomon error correction (bitrot protection). It can
	// only be chosen when the repository is created.
	ECC bool `json:"ecc"`
}

// SFTP covers a Hetzner Storage Box (port 23) or any SFTP server.
type SFTP struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	Path string `json:"path"`
	// KeyFile is a private key path on the agent host.
	KeyFile string `json:"key_file,omitempty"`
	// KeyFiles overrides KeyFile for individual agents (agent ID -> path), so
	// agents sharing a repository can each use their own key. Agents only ever
	// receive the one path that applies to them (see ForAgent).
	KeyFiles map[string]string `json:"key_files,omitempty"`
	Password string            `json:"password,omitempty"`
	// KnownHosts is known_hosts content. Empty at creation means trust on
	// first use: the agent records the host key it sees.
	KnownHosts string `json:"known_hosts,omitempty"`
}

type S3 struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Prefix          string `json:"prefix,omitempty"`
	Region          string `json:"region,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	Insecure        bool   `json:"insecure,omitempty"` // plain HTTP, for a local test server
}

// Filesystem is a directory on the agent host (local disk or a mount).
type Filesystem struct {
	Path string `json:"path"`
	// ServerPath is optional: where the same directory is mounted in the
	// server's container. Only the server uses it (so it can run maintenance);
	// agents never see it and it is not part of Fingerprint.
	ServerPath string `json:"server_path,omitempty"`
}

func (c Config) Validate() error {
	switch c.Kind {
	case KindSFTP:
		s := c.SFTP
		if s == nil || s.Host == "" || s.User == "" || s.Path == "" {
			return errors.New("sftp needs host, user and path")
		}
		if s.Port <= 0 || s.Port > 65535 {
			return fmt.Errorf("sftp port %d is invalid", s.Port)
		}
		if s.KeyFile == "" && s.Password == "" && len(s.KeyFiles) == 0 {
			return errors.New("sftp needs a key file or a password")
		}
	case KindS3:
		s := c.S3
		if s == nil || s.Endpoint == "" || s.Bucket == "" || s.AccessKeyID == "" || s.SecretAccessKey == "" {
			return errors.New("s3 needs endpoint, bucket, access key ID and secret key")
		}
	case KindFilesystem:
		if c.Filesystem == nil || !path.IsAbs(c.Filesystem.Path) {
			return errors.New("filesystem needs an absolute path")
		}
		if sp := c.Filesystem.ServerPath; sp != "" && !path.IsAbs(sp) {
			return errors.New("the path on the server must be absolute")
		}
	default:
		return fmt.Errorf("unknown repository kind %q", c.Kind)
	}
	return nil
}

// Redacted returns a copy without secrets, for display and the JSON API.
func (c Config) Redacted() Config {
	if c.SFTP != nil {
		s := *c.SFTP
		if s.Password != "" {
			s.Password = "***"
		}
		c.SFTP = &s
	}
	if c.S3 != nil {
		s := *c.S3
		if s.SecretAccessKey != "" {
			s.SecretAccessKey = "***"
		}
		c.S3 = &s
	}
	return c
}

// Location is a one-line human description.
func (c Config) Location() string {
	switch {
	case c.SFTP != nil:
		return fmt.Sprintf("sftp://%s@%s:%d%s", c.SFTP.User, c.SFTP.Host, c.SFTP.Port, c.SFTP.Path)
	case c.S3 != nil:
		return "s3://" + strings.TrimSuffix(c.S3.Bucket+"/"+c.S3.Prefix, "/") + " @ " + c.S3.Endpoint
	case c.Filesystem != nil:
		return c.Filesystem.Path
	}
	return c.Kind
}

// Identity names the storage location the config points at, normalised so two
// configs for the same place compare equal: the host in lower case, paths
// cleaned, an S3 prefix without its slashes. Credentials, key files and the
// server-side path are not part of it. Two repositories must not share one.
func (c Config) Identity() string {
	switch {
	case c.SFTP != nil:
		return fmt.Sprintf("sftp|%s|%d|%s|%s", strings.ToLower(c.SFTP.Host), c.SFTP.Port, c.SFTP.User, path.Clean(c.SFTP.Path))
	case c.S3 != nil:
		ep := strings.TrimRight(strings.ToLower(c.S3.Endpoint), "/")
		return "s3|" + ep + "|" + c.S3.Bucket + "|" + strings.Trim(c.S3.Prefix, "/")
	case c.Filesystem != nil:
		return "fs|" + path.Clean(c.Filesystem.Path)
	}
	return ""
}

// SharesS3Listing says whether c and o are S3 repositories in one bucket whose
// prefixes are different but list each other's objects. A prefix is a plain
// string, not a directory: "nas" also matches every object of "nas2" or
// "nas-old", so maintenance or a wipe of one repository would see the other's
// blobs as its own. Prefixes are apart once a "/" separates them ("nas/" and
// "nas2/", or "" and "nas/"). The same prefix is not this case: Identity
// covers it.
func (c Config) SharesS3Listing(o Config) bool {
	if c.S3 == nil || o.S3 == nil || c.S3.Bucket != o.S3.Bucket ||
		!strings.EqualFold(strings.TrimRight(c.S3.Endpoint, "/"), strings.TrimRight(o.S3.Endpoint, "/")) {
		return false
	}
	short, long := c.S3.Prefix, o.S3.Prefix
	if len(short) > len(long) {
		short, long = long, short
	}
	rest, ok := strings.CutPrefix(long, short)
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// ForAgent is the config as the given agent sees it: without the server-only
// path, and with the SFTP key path that applies to that agent (its entry in
// KeyFiles, else the default). An empty agentID gives the default for all.
func (c Config) ForAgent(agentID string) Config {
	if c.Filesystem != nil && c.Filesystem.ServerPath != "" {
		f := *c.Filesystem
		f.ServerPath = ""
		c.Filesystem = &f
	}
	if c.SFTP != nil && len(c.SFTP.KeyFiles) > 0 {
		s := *c.SFTP
		if k := s.KeyFiles[agentID]; k != "" {
			s.KeyFile = k
		}
		s.KeyFiles = nil
		c.SFTP = &s
	}
	return c
}

// ForServer is the config as the server opens it. A filesystem repository is
// reached through its server path; without one the server cannot open it.
func (c Config) ForServer() (Config, error) {
	if c.Filesystem == nil {
		return c, nil
	}
	if c.Filesystem.ServerPath == "" {
		return c, errors.New("this directory is on an agent host; set its path on the server (where it is mounted in the server container) first")
	}
	f := Filesystem{Path: c.Filesystem.ServerPath}
	c.Filesystem = &f
	return c, nil
}

// Fingerprint changes whenever the storage settings change, so agents can
// drop a stale local connection. It ignores the server-only path and the
// per-agent key table (an agent fingerprints the config it was sent).
func (c Config) Fingerprint() string {
	b, _ := json.Marshal(c.ForAgent(""))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}
