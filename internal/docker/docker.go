// Package docker is a minimal Docker Engine API client over the Unix
// socket: list containers, pause/stop/start them, and exec with streamed
// stdin/stdout. It deliberately avoids the Docker SDK's dependency tree.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	socket string
	hc     *http.Client
}

func New(socket string) *Client {
	return &Client{
		socket: socket,
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		}},
	}
}

type Mount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Mounts []Mount           `json:"Mounts"`
}

// Name is the container name without Docker's leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers lists all containers, running or not.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var out []Container
	err := c.do(ctx, "GET", "/containers/json?all=1", nil, &out)
	return out, err
}

func (c *Client) Pause(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/pause", nil, nil)
}

func (c *Client) Unpause(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/unpause", nil, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/stop?t=60", nil, nil)
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil // already paused/stopped/started
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Exec runs cmd in a running container. stdin (optional) is streamed to the
// command. The returned reader yields the command's stdout; once stdout ends
// it returns io.EOF only if the command exited 0, otherwise an error
// carrying the exit code and the tail of stderr.
func (c *Client) Exec(ctx context.Context, containerID string, cmd, env []string, stdin io.Reader) (io.ReadCloser, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, "POST", "/containers/"+url.PathEscape(containerID)+"/exec", map[string]any{
		"AttachStdin":  stdin != nil,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          cmd,
		"Env":          env,
	}, &created); err != nil {
		return nil, err
	}

	conn, br, err := c.hijack(ctx, "/exec/"+created.ID+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })

	if stdin != nil {
		go func() {
			io.Copy(conn, stdin)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}()
	}

	pr, pw := io.Pipe()
	go func() {
		defer stop()
		defer conn.Close()
		var stderr tailBuffer
		err := demux(br, pw, &stderr)
		if err == nil {
			err = c.execResult(ctx, created.ID, cmd, &stderr)
		}
		pw.CloseWithError(err) // nil closes with io.EOF
	}()
	return &execReader{PipeReader: pr, conn: conn}, nil
}

func (c *Client) execResult(ctx context.Context, execID string, cmd []string, stderr *tailBuffer) error {
	// The exec can take a moment to report it has exited after the stream ends.
	for i := 0; i < 50; i++ {
		var st struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := c.do(context.WithoutCancel(ctx), "GET", "/exec/"+execID+"/json", nil, &st); err != nil {
			return err
		}
		if !st.Running {
			if st.ExitCode != 0 {
				return fmt.Errorf("%s exited with %d: %s", cmdName(cmd), st.ExitCode, strings.TrimSpace(stderr.String()))
			}
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s: exec did not report an exit code", cmdName(cmd))
}

type execReader struct {
	*io.PipeReader
	conn net.Conn
}

func (r *execReader) Close() error {
	r.conn.Close()
	return r.PipeReader.Close()
}

// hijack sends a POST that Docker upgrades to a raw bidirectional stream.
func (c *Client) hijack(ctx context.Context, path string, body any) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, nil, fmt.Errorf("docker: %w", err)
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", "http://docker"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("docker POST %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, nil, fmt.Errorf("docker POST %s: %s: %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return conn, br, nil
}

// demux splits Docker's multiplexed stream: an 8-byte header (stream type,
// 3 zero bytes, big-endian length) before each frame.
func demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker stream: %w", err)
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		var dst io.Writer
		switch hdr[0] {
		case 1:
			dst = stdout
		case 2:
			dst = stderr
		default:
			dst = io.Discard
		}
		if _, err := io.CopyN(dst, r, n); err != nil {
			return fmt.Errorf("docker stream: %w", err)
		}
	}
}

// tailBuffer keeps the last 4 KiB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

func cmdName(cmd []string) string {
	if len(cmd) == 0 {
		return "exec"
	}
	return cmd[0]
}
