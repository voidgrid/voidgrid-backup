// Package dockertest serves a fake Docker Engine API on a Unix socket, with
// real connection hijacking and stream multiplexing for exec.
package dockertest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/docker"
)

// ExecFunc plays the command. It returns the exit code.
type ExecFunc func(container string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int

type Fake struct {
	Socket string
	Exec   ExecFunc

	mu         sync.Mutex
	containers []docker.Container
	calls      []string
	execs      map[string]*execState
	nextExec   int
}

type execState struct {
	container string
	cmd       []string
	stdin     bool
	running   bool
	exit      int
}

// New starts a fake daemon with the given containers.
func New(t *testing.T, containers ...docker.Container) *Fake {
	t.Helper()
	dir, err := os.MkdirTemp("", "dk") // short: socket paths are limited to ~108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &Fake{Socket: filepath.Join(dir, "docker.sock"), containers: containers, execs: map[string]*execState{}}
	lis, err := net.Listen("unix", f.Socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", f.list)
	mux.HandleFunc("POST /containers/{id}/{action}", f.action)
	mux.HandleFunc("POST /exec/{id}/start", f.start)
	mux.HandleFunc("GET /exec/{id}/json", f.inspect)
	srv := &http.Server{Handler: mux}
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Close() })
	return f
}

// Calls returns the pause/unpause/stop/start calls made, e.g. "pause app".
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// State returns a container's current state.
func (f *Fake) State(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.ID == id {
			return c.State
		}
	}
	return ""
}

func (f *Fake) list(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	json.NewEncoder(w).Encode(f.containers)
}

func (f *Fake) action(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	f.mu.Lock()
	idx := -1
	for i, c := range f.containers {
		if c.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		f.mu.Unlock()
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
		return
	}
	if action == "exec" {
		var body struct {
			Cmd         []string
			AttachStdin bool
		}
		json.NewDecoder(r.Body).Decode(&body)
		if f.containers[idx].State != "running" {
			f.mu.Unlock()
			http.Error(w, `{"message":"container is not running"}`, http.StatusConflict)
			return
		}
		f.nextExec++
		eid := fmt.Sprintf("exec%d", f.nextExec)
		f.execs[eid] = &execState{container: id, cmd: body.Cmd, stdin: body.AttachStdin}
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"Id": eid})
		return
	}
	defer f.mu.Unlock()
	st := &f.containers[idx].State
	f.calls = append(f.calls, action+" "+id)
	switch action {
	case "pause":
		if *st != "running" {
			http.Error(w, `{"message":"not running"}`, http.StatusConflict)
			return
		}
		*st = "paused"
	case "unpause":
		*st = "running"
	case "stop":
		if *st == "exited" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		*st = "exited"
	case "start":
		if *st == "running" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		*st = "running"
	default:
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *Fake) start(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ex := f.execs[r.PathValue("id")]
	if ex != nil {
		ex.running = true
	}
	f.mu.Unlock()
	if ex == nil {
		http.NotFound(w, r)
		return
	}
	io.Copy(io.Discard, r.Body) // Docker consumes the start options before upgrading
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	rw.Flush()

	var mu sync.Mutex
	frame := func(stream byte) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			var hdr [8]byte
			hdr[0] = stream
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(p)))
			if _, err := conn.Write(hdr[:]); err != nil {
				return 0, err
			}
			return conn.Write(p)
		})
	}
	var stdin io.Reader = strings.NewReader("")
	if ex.stdin {
		stdin = rw.Reader
	}
	code := 0
	if f.Exec != nil {
		code = f.Exec(ex.container, ex.cmd, stdin, frame(1), frame(2))
	}
	f.mu.Lock()
	ex.running, ex.exit = false, code
	f.mu.Unlock()
}

func (f *Fake) inspect(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex := f.execs[r.PathValue("id")]
	if ex == nil {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"Running": ex.running, "ExitCode": ex.exit})
}

type writerFunc func([]byte) (int, error)

func (fn writerFunc) Write(p []byte) (int, error) { return fn(p) }

// ComposeContainer builds a container carrying compose labels.
func ComposeContainer(id, project, workdir, service, image, state string, mounts ...docker.Mount) docker.Container {
	return docker.Container{
		ID: id, Names: []string{"/" + project + "-" + service + "-1"}, Image: image, State: state,
		Labels: map[string]string{
			docker.LabelProject:     project,
			docker.LabelWorkingDir:  workdir,
			docker.LabelConfigFiles: workdir + "/docker-compose.yaml",
			docker.LabelService:     service,
		},
		Mounts: mounts,
	}
}

// Bind is a read-write bind mount.
func Bind(source, dest string) docker.Mount {
	return docker.Mount{Type: "bind", Source: source, Destination: dest, RW: true}
}
