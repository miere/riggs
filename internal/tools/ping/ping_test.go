package ping

import (
	"context"
	"testing"
)

func TestInvoke(t *testing.T) {
	got, err := New().Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got != "pong" {
		t.Errorf("Invoke = %v, want pong", got)
	}
}

// A nil schema is the contract for "takes no parameters"; the frontends rely
// on it to decide what to publish and what to reject.
func TestNoParameters(t *testing.T) {
	if s := New().InputSchema(); s != nil {
		t.Errorf("InputSchema = %v, want nil", s)
	}
	if name := New().Name(); name != "ping" {
		t.Errorf("Name = %q, want ping", name)
	}
}
