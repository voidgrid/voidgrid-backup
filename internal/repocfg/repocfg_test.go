package repocfg

import "testing"

func TestForAgentResolvesTheKey(t *testing.T) {
	c := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFile: "/keys/default",
		KeyFiles: map[string]string{"a1": "/keys/a1"}}}
	if got := c.ForAgent("a1").SFTP.KeyFile; got != "/keys/a1" {
		t.Errorf("a1 key = %q", got)
	}
	if got := c.ForAgent("a2").SFTP.KeyFile; got != "/keys/default" {
		t.Errorf("a2 key = %q, want the default", got)
	}
	if c.ForAgent("a1").SFTP.KeyFiles != nil {
		t.Error("an agent was sent the table of every agent's key")
	}
	if c.SFTP.KeyFile != "/keys/default" || c.SFTP.KeyFiles == nil {
		t.Error("ForAgent changed the original")
	}
	// A config that never used the table fingerprints as before.
	plain := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFile: "/keys/default"}}
	if plain.Fingerprint() != c.Fingerprint() {
		t.Error("the key table changed the fingerprint of the default config")
	}
	// Only per-agent keys is a valid repository.
	onlyTable := Config{Kind: KindSFTP, SFTP: &SFTP{Host: "h", Port: 23, User: "u", Path: "p", KeyFiles: map[string]string{"a1": "/k"}}}
	if err := onlyTable.Validate(); err != nil {
		t.Errorf("per-agent keys only: %v", err)
	}
}

func TestSharesS3Listing(t *testing.T) {
	s3 := func(bucket, prefix string) Config {
		return Config{Kind: KindS3, S3: &S3{Endpoint: "s3.example.com", Bucket: bucket, Prefix: prefix, AccessKeyID: "k", SecretAccessKey: "s"}}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"nas", "nas2", true},
		{"nas2", "nas", true},
		{"", "nas", true}, // the bucket root lists every object that is not under a "/"
		{"nas-backup", "nas-backup-old", true},
		{"nas/", "nas2/", false},
		{"nas", "nas/sub", false},
		{"", "nas/", false},
		{"nas", "other", false},
		{"nas", "nas", false}, // the same place: Identity refuses that
	} {
		if got := s3("b", tc.a).SharesS3Listing(s3("b", tc.b)); got != tc.want {
			t.Errorf("%q and %q: %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	if s3("b", "nas").SharesS3Listing(s3("other-bucket", "nas2")) {
		t.Error("different buckets share nothing")
	}
	fs := Config{Kind: KindFilesystem, Filesystem: &Filesystem{Path: "/nas"}}
	if fs.SharesS3Listing(s3("b", "nas2")) || s3("b", "nas").SharesS3Listing(fs) {
		t.Error("only S3 repositories can share a listing")
	}
}
