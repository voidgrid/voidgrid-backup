package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo/maintenance"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newEngine(t *testing.T, host string) *Engine {
	t.Helper()
	e, err := New(filepath.Join(t.TempDir(), "agent"), host)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

var keepOne = Retention{Latest: 1}

func TestFilesystemRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	writeFile(t, filepath.Join(src, "sub", "b.txt"), "bee")
	writeFile(t, filepath.Join(src, "sub", "skip.log"), "noise")

	e := newEngine(t, "host-a")
	r := Repo{ID: "r1", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, ECC: true,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")},
	}}
	if created, _, err := e.Init(ctx, r); err != nil || !created {
		t.Fatalf("Init: created=%v err=%v", created, err)
	}
	if created, _, err := e.Init(ctx, r); err != nil || created {
		t.Fatalf("second Init: created=%v err=%v", created, err)
	}

	res, err := e.Backup(ctx, r, []string{src}, []string{"*.log"}, keepOne)
	if err != nil || len(res) != 1 || res[0].Err != nil || res[0].SnapshotID == "" || res[0].Files != 2 {
		t.Fatalf("first backup: %+v %v", res, err)
	}
	writeFile(t, filepath.Join(src, "a.txt"), "two")
	res, err = e.Backup(ctx, r, []string{src}, []string{"*.log"}, keepOne)
	if err != nil || res[0].Err != nil || res[0].Pruned != 1 {
		t.Fatalf("second backup: %+v %v", res, err)
	}

	snaps, err := e.ListSnapshots(ctx, r, []string{src})
	if err != nil || len(snaps) != 1 || snaps[0].ID != res[0].SnapshotID {
		t.Fatalf("snapshots after keep-latest-1: %+v %v", snaps, err)
	}
	if all, err := e.ListSnapshots(ctx, r, nil); err != nil || len(all) != 1 {
		t.Fatalf("all snapshots: %+v %v", all, err)
	}

	root, err := e.ListDirectory(ctx, r, snaps[0].ID, "")
	if err != nil || len(root) != 2 || !root[0].Dir || root[0].Name != "sub" || root[1].Name != "a.txt" {
		t.Fatalf("root listing: %+v %v", root, err)
	}
	sub, err := e.ListDirectory(ctx, r, snaps[0].ID, "sub")
	if err != nil || len(sub) != 1 || sub[0].Name != "b.txt" {
		t.Fatalf("sub listing (the .log must be excluded): %+v %v", sub, err)
	}
	if _, err := e.ListDirectory(ctx, r, snaps[0].ID, "../etc"); err == nil {
		t.Fatal("listing with .. succeeded")
	}

	target := filepath.Join(t.TempDir(), "restore")
	st, err := e.Restore(ctx, r, snaps[0].ID, "", target, false)
	if err != nil || st.Files != 2 {
		t.Fatalf("restore: %+v %v", st, err)
	}
	if got := readFile(t, filepath.Join(target, "a.txt")); got != "two" {
		t.Fatalf("restored a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "sub", "b.txt")); got != "bee" {
		t.Fatalf("restored sub/b.txt = %q", got)
	}

	// Without overwrite a non-empty target is refused up front.
	writeFile(t, filepath.Join(target, "a.txt"), "local edit")
	if _, err := e.Restore(ctx, r, snaps[0].ID, "", target, false); err == nil {
		t.Fatal("restore without overwrite into a non-empty directory succeeded")
	}
	if got := readFile(t, filepath.Join(target, "a.txt")); got != "local edit" {
		t.Fatalf("refused restore still changed a.txt to %q", got)
	}
	if _, err := e.Restore(ctx, r, snaps[0].ID, "", target, true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(target, "a.txt")); got != "two" {
		t.Fatalf("restore with overwrite left a.txt = %q", got)
	}

	// A subdirectory on its own.
	target2 := filepath.Join(t.TempDir(), "sub-only")
	if _, err := e.Restore(ctx, r, snaps[0].ID, "sub", target2, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(target2, "b.txt")); got != "bee" {
		t.Fatalf("sub-only restore b.txt = %q", got)
	}

	if _, err := e.Restore(ctx, r, snaps[0].ID, "", "/", false); err == nil {
		t.Fatal("restore to / allowed")
	}
	if res, err := e.Backup(ctx, r, []string{"/"}, nil, keepOne); err != nil || res[0].Err == nil {
		t.Fatalf("backup of / allowed: %+v %v", res, err)
	}

	// Another host can't browse this host's snapshots.
	other := newEngine(t, "host-b")
	if _, err := other.ListDirectory(ctx, r, snaps[0].ID, ""); err == nil {
		t.Fatal("host-b browsed host-a's snapshot")
	}
	// Wrong password.
	bad := r
	bad.Password = "wrong"
	if _, err := newEngine(t, "host-a").ListSnapshots(ctx, bad, nil); err == nil {
		t.Fatal("opened the repository with the wrong password")
	}
}

func TestMaintenanceOwner(t *testing.T) {
	ctx := context.Background()
	r := Repo{ID: "r", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")},
	}}
	creator := newEngine(t, "host-a")
	if _, _, err := creator.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	rep, err := creator.open(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	p, err := maintenance.GetParams(ctx, rep)
	rep.Close(ctx)
	if err != nil || p.Owner != Username+"@host-a" {
		t.Fatalf("maintenance owner = %q, %v", p.Owner, err)
	}

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "x"), "x")
	for _, e := range []*Engine{creator, newEngine(t, "host-b")} {
		res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
		if err != nil || res[0].Err != nil || len(res[0].Warnings) != 0 {
			t.Fatalf("%s backup: %+v %v", e.hostname, res, err)
		}
	}
}

func TestExcludesOwnCache(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "keep.txt"), "x")
	e, err := New(filepath.Join(src, "agent-data"), "h")
	if err != nil {
		t.Fatal(err)
	}
	r := Repo{ID: "r", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")},
	}}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || res[0].Err != nil {
		t.Fatalf("%+v %v", res, err)
	}
	root, err := e.ListDirectory(ctx, r, res[0].SnapshotID, "")
	if err != nil || len(root) != 1 || root[0].Name != "keep.txt" {
		t.Fatalf("agent data dir was not excluded: %+v %v", root, err)
	}
}

func TestSFTPRoundTrip(t *testing.T) {
	ctx := context.Background()
	port, keyFile := startSFTPServer(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), "over sftp")

	e := newEngine(t, "host-a")
	r := Repo{ID: "box", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindSFTP, ECC: true,
		SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: t.TempDir(), KeyFile: keyFile},
	}}
	created, kh, err := e.Init(ctx, r)
	if err != nil || !created || kh == "" {
		t.Fatalf("Init over sftp: created=%v knownHosts=%q err=%v", created, kh, err)
	}
	r.Config.SFTP.KnownHosts = kh // the server pins what the agent saw

	res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup over sftp: %+v %v", res, err)
	}
	target := filepath.Join(t.TempDir(), "out")
	if _, err := e.Restore(ctx, r, res[0].SnapshotID, "", target, false); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(target, "f.txt")); got != "over sftp" {
		t.Fatalf("restored f.txt = %q", got)
	}

	// A different pinned host key must be refused.
	otherPort, _ := startSFTPServer(t)
	otherKH, err := FetchHostKey(ctx, "127.0.0.1", otherPort)
	if err != nil {
		t.Fatal(err)
	}
	wrong := r
	wrongCfg := *r.Config.SFTP
	wrongCfg.KnownHosts = replacePort(otherKH, otherPort, port)
	wrong.Config.SFTP = &wrongCfg
	if _, err := newEngine(t, "host-a").ListSnapshots(ctx, wrong, nil); err == nil {
		t.Fatal("connected despite a mismatched host key")
	}
}

// replacePort rewrites the [127.0.0.1]:port marker in a known_hosts line so a
// key from one test server is presented as belonging to another.
func replacePort(line string, from, to int) string {
	return string(bytes.Replace([]byte(line),
		[]byte(net.JoinHostPort("127.0.0.1", itoa(from))),
		[]byte(net.JoinHostPort("127.0.0.1", itoa(to))), 1))
}

func itoa(n int) string { return string(appendInt(nil, n)) }

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

// startSFTPServer runs an in-process SSH server with the sftp subsystem,
// accepting one generated client key. It stands in for a Storage Box.
func startSFTPServer(t *testing.T) (port int, keyFile string) {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	allowed, err := ssh.NewPublicKey(clientPub)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("key not allowed")
		},
	}
	cfg.AddHostKey(hostSigner)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go serveSSH(c, cfg)
		}
	}()
	return lis.Addr().(*net.TCPAddr).Port, keyFile
}

func serveSSH(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range creqs {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				req.Reply(ok, nil)
				if ok {
					go func() {
						srv, err := sftp.NewServer(ch)
						if err == nil {
							srv.Serve()
							srv.Close()
						}
						ch.Close()
					}()
				}
			}
		}()
	}
}
