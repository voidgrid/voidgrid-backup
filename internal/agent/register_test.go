package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRegisterRejectsMalformedToken(t *testing.T) {
	a, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	err = a.Register(context.Background(), RegisterConfig{Server: "127.0.0.1:1", Token: "not-a-token"})
	if err == nil || !strings.Contains(err.Error(), "VB_TOKEN") {
		t.Fatalf("malformed token: %v", err)
	}
}

func TestRegisterStopsWithContext(t *testing.T) {
	a, err := New(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	// A well-formed token for a server that isn't there: Register retries
	// until the context ends instead of giving up or hanging.
	tok := "vbr1-" + strings.Repeat("a", 32) + "-" + strings.Repeat("0", 32)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = a.Register(ctx, RegisterConfig{Server: "127.0.0.1:1", Token: tok, ListenPort: 9443,
		PollEvery: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("Register returned %v before its context ended", err)
	}
	if a.ID() != "" {
		t.Fatal("agent enrolled without a server")
	}
}
