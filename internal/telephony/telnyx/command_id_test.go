package telnyx

import "testing"

func TestCommandIDIsStableAndPerCall(t *testing.T) {
	a := CommandID("call_a")
	if a != CommandID("call_a") {
		t.Fatal("same call produced different command ids")
	}
	if a == CommandID("call_b") {
		t.Fatal("different calls produced the same command id")
	}
	if len(a) != 32 {
		t.Fatalf("command id length = %d, want 32", len(a))
	}
}
