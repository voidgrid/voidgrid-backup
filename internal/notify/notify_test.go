package notify

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

func testEvent() Event {
	return Event{Job: "docs", Kind: "backup", Status: "failed", Summary: "1 of 2 paths", Error: "boom", JobID: "abc123"}
}

func TestSendSkipsDisabledAndNilChannels(t *testing.T) {
	if results := Send(context.Background(), Config{}, testEvent()); len(results) != 0 {
		t.Fatalf("empty config sent: %+v", results)
	}
	cfg := Config{Discord: &DiscordConfig{Enabled: false, WebhookURL: "http://unused"}}
	if results := Send(context.Background(), cfg, testEvent()); len(results) != 0 {
		t.Fatalf("disabled channel sent: %+v", results)
	}
}

func TestSendNotifarr(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type: %s", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode the request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	notifarrURL = srv.URL + "/api/v1/notification/passthrough/"
	defer func() { notifarrURL = defaultNotifarrURL }()

	cfg := Config{URL: "https://backup.example", Notifarr: &NotifarrConfig{
		Enabled: true, APIKey: "test-key", ChannelID: 123456789, Color: "b3261e",
	}}
	results := Send(context.Background(), cfg, testEvent())
	if len(results) != 1 || results[0].Channel != "Notifarr" || results[0].Err != nil {
		t.Fatalf("results: %+v", results)
	}
	if !strings.HasSuffix(gotPath, "/test-key") {
		t.Fatalf("path missing api key: %s", gotPath)
	}
	discord, _ := gotBody["discord"].(map[string]any)
	ids, _ := discord["ids"].(map[string]any)
	if ids["channel"].(float64) != 123456789 {
		t.Fatalf("channel id: %+v", discord)
	}
	text, _ := discord["text"].(map[string]any)
	if !strings.Contains(text["description"].(string), "backup.example/jobs/abc123") {
		t.Fatalf("no job link in body: %+v", text)
	}
}

func TestSendNotifarrHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad api key", http.StatusUnauthorized)
	}))
	defer srv.Close()
	notifarrURL = srv.URL + "/api/v1/notification/passthrough/"
	defer func() { notifarrURL = defaultNotifarrURL }()

	cfg := Config{Notifarr: &NotifarrConfig{Enabled: true, APIKey: "bad", ChannelID: 1}}
	results := Send(context.Background(), cfg, testEvent())
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("expected an error result: %+v", results)
	}
}

func TestSendDiscord(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode the request body: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := Config{Discord: &DiscordConfig{Enabled: true, WebhookURL: srv.URL}}
	results := Send(context.Background(), cfg, testEvent())
	if len(results) != 1 || results[0].Channel != "Discord" || results[0].Err != nil {
		t.Fatalf("results: %+v", results)
	}
	embeds, _ := gotBody["embeds"].([]any)
	if len(embeds) != 1 {
		t.Fatalf("embeds: %+v", gotBody)
	}
	embed := embeds[0].(map[string]any)
	if !strings.Contains(embed["title"].(string), "docs backup: failed") {
		t.Fatalf("title: %+v", embed)
	}
	if embed["color"].(float64) != 0xb3261e {
		t.Fatalf("failed status should use the red color: %+v", embed)
	}
}

// fakeSMTP is a minimal SMTP server: enough protocol to exercise
// sendEmail's plaintext and AUTH PLAIN paths without needing a TLS
// listener (STARTTLS/implicit TLS themselves are net/smtp's own well-tested
// code, not this package's).
type fakeSMTP struct {
	authLine string // the base64 AUTH PLAIN argument it received, if any
	mailFrom string
	rcptTo   []string
	data     string
}

func startFakeSMTP(t *testing.T, withAuth bool) (addr string, got *fakeSMTP) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tidy.CloseWarn("lis", lis) })
	got = &fakeSMTP{}
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer tidy.CloseWarn("conn", conn)
		r := bufio.NewReader(conn)
		reply := func(s string) {
			_, err := conn.Write([]byte(s + "\r\n"))
			tidy.Check("fake smtp reply", err)
		}
		reply("220 fake.smtp ready")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "EHLO"):
				if withAuth {
					reply("250-fake.smtp")
					reply("250 AUTH PLAIN")
				} else {
					reply("250 fake.smtp")
				}
			case strings.HasPrefix(line, "AUTH PLAIN "):
				got.authLine = strings.TrimPrefix(line, "AUTH PLAIN ")
				reply("235 authenticated")
			case strings.HasPrefix(line, "MAIL FROM:"):
				got.mailFrom = line
				reply("250 ok")
			case strings.HasPrefix(line, "RCPT TO:"):
				got.rcptTo = append(got.rcptTo, line)
				reply("250 ok")
			case line == "DATA":
				reply("354 go ahead")
				var b strings.Builder
				for {
					dl, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(dl, "\r\n") == "." {
						break
					}
					b.WriteString(dl)
				}
				got.data = b.String()
				reply("250 ok")
			case line == "QUIT":
				reply("221 bye")
				return
			default:
				reply("500 unrecognized")
			}
		}
	}()
	return lis.Addr().String(), got
}

func TestSendEmail(t *testing.T) {
	addr, got := startFakeSMTP(t, false)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	cfg := Config{URL: "https://backup.example", Email: &EmailConfig{
		Enabled: true, Host: host, Port: port, From: "vb@example.com", To: []string{"me@example.com"},
	}}
	results := Send(context.Background(), cfg, testEvent())
	if len(results) != 1 || results[0].Channel != "Email" || results[0].Err != nil {
		t.Fatalf("results: %+v", results)
	}
	if !strings.Contains(got.mailFrom, "vb@example.com") {
		t.Fatalf("mail from: %q", got.mailFrom)
	}
	if len(got.rcptTo) != 1 || !strings.Contains(got.rcptTo[0], "me@example.com") {
		t.Fatalf("rcpt to: %+v", got.rcptTo)
	}
	if !strings.Contains(got.data, "Subject: docs backup: failed") || !strings.Contains(got.data, "boom") {
		t.Fatalf("message body: %q", got.data)
	}
}

func TestSendEmailAuth(t *testing.T) {
	addr, got := startFakeSMTP(t, true)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	cfg := Config{Email: &EmailConfig{
		Enabled: true, Host: host, Port: port, Username: "user", Password: "pw",
		From: "vb@example.com", To: []string{"me@example.com"},
	}}
	results := Send(context.Background(), cfg, testEvent())
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results: %+v", results)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.authLine)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(decoded); !strings.Contains(s, "\x00user\x00pw") {
		t.Fatalf("AUTH PLAIN payload: %q", s)
	}
}

// What a service answers is kept, and a failure never carries the URL, which
// holds the API key or the webhook's token.
func TestPostJSONKeepsTheAnswerAndHidesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ok") {
			_, _ = w.Write([]byte(`{"result":"success"}`))
			return
		}
		http.Error(w, "no such key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	d, err := postJSON(context.Background(), srv.URL+"/secret-token/ok", map[string]any{})
	if err != nil || d != `{"result":"success"}` {
		t.Fatalf("ok: %q %v", d, err)
	}
	_, err = postJSON(context.Background(), srv.URL+"/secret-token/bad", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "no such key") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("http error: %v", err)
	}
	url := srv.URL + "/secret-token/ok"
	srv.Close()
	_, err = postJSON(context.Background(), url, map[string]any{})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("a connection error must not carry the URL: %v", err)
	}
}

// A mail server that accepts the connection and then says nothing must not hold
// the sender: a run reports its outcome before it lets go of its job.
func TestSendEmailGivesUpOnASilentServer(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	over := make(chan struct{})
	t.Cleanup(func() {
		close(over)
		tidy.CloseWarn("listener", lis)
	})
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { // held open and never answered, until the test is over
				<-over
				tidy.Close(c)
			}()
		}
	}()
	old := emailTimeout
	emailTimeout = 300 * time.Millisecond
	t.Cleanup(func() { emailTimeout = old })

	port := lis.Addr().(*net.TCPAddr).Port
	cfg := Config{Email: &EmailConfig{Enabled: true, Host: "127.0.0.1", Port: port, From: "vb@example.com", To: []string{"me@example.com"}}}
	done := make(chan []Result, 1)
	go func() { done <- Send(context.Background(), cfg, testEvent()) }()
	select {
	case results := <-done:
		if len(results) != 1 || results[0].Err == nil {
			t.Fatalf("results: %+v, want one failed email", results)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send is still waiting for a mail server that never answers")
	}
}

// A line break in a job name must not start a mail header of its own.
func TestSendEmailKeepsTheSubjectOnOneLine(t *testing.T) {
	addr, got := startFakeSMTP(t, false)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	cfg := Config{Email: &EmailConfig{Enabled: true, Host: host, Port: port, From: "vb@example.com", To: []string{"me@example.com"}}}
	ev := testEvent()
	ev.Job = "docs\r\nBcc: someone@example.com\r\n\r\ninjected body"
	if results := Send(context.Background(), cfg, ev); len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results: %+v", results)
	}
	head, _, _ := strings.Cut(got.data, "\r\n\r\n")
	if strings.Contains(head, "\nBcc:") || !strings.Contains(head, "Subject: docs Bcc: someone@example.com injected body backup: failed") {
		t.Fatalf("headers: %q", head)
	}
}
