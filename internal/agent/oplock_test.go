package agent

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// An operation waiting for its turn gives up when it is cancelled, and the
// turn still goes to the next one in line afterwards.
func TestOpLockWaiterGivesUpWhenCancelled(t *testing.T) {
	var l opLock
	unlock, err := l.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := l.lock(ctx)
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a second operation did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-got:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled operation is still waiting for its turn")
	}
	unlock()
	if _, err := l.lock(ctx); status.Code(err) != codes.Canceled {
		t.Fatalf("an operation that is already cancelled took the turn: %v", err)
	}
	unlock2, err := l.lock(context.Background())
	if err != nil {
		t.Fatalf("the turn was not given back: %v", err)
	}
	unlock2()
}
