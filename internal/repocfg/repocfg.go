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
	KeyFile  string `json:"key_file,omitempty"`
	Password string `json:"password,omitempty"`
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
		if s.KeyFile == "" && s.Password == "" {
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

// Fingerprint changes whenever the storage settings change, so agents can
// drop a stale local connection.
func (c Config) Fingerprint() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}
