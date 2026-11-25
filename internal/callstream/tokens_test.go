package callstream

import (
	"strings"
	"testing"
)

func key() []byte { return []byte(strings.Repeat("k", 32)) }

func TestTokenIsStableAndCallBound(t *testing.T) {
	tk, err := NewTokens(key())
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := tk.Token("call-a")
	a2, _ := tk.Token("call-a")
	b, _ := tk.Token("call-b")
	if a1 != a2 {
		t.Fatal("token must be stable for a call, so a restart keeps the stream URL valid")
	}
	if a1 == b {
		t.Fatal("tokens for different calls must differ")
	}
	if !tk.Valid("call-a", a1) {
		t.Fatal("own token must validate")
	}
	if tk.Valid("call-b", a1) {
		t.Fatal("a token must not validate for another call")
	}
}

func TestTokenDependsOnMasterKey(t *testing.T) {
	a, _ := NewTokens(key())
	other := []byte(strings.Repeat("z", 32))
	b, _ := NewTokens(other)
	ta, _ := a.Token("call")
	if b.Valid("call", ta) {
		t.Fatal("a token from one master key must not validate under another")
	}
}

func TestRejectsEmptyAndShortInput(t *testing.T) {
	if _, err := NewTokens([]byte("short")); err == nil {
		t.Fatal("short master key must be refused")
	}
	tk, _ := NewTokens(key())
	if _, err := tk.Token(""); err == nil {
		t.Fatal("empty call id must be refused")
	}
	if tk.Valid("", "") || tk.Valid("call", "") {
		t.Fatal("empty input must never validate")
	}
}

func TestTokenIsURLSafe(t *testing.T) {
	tk, _ := NewTokens(key())
	tok, _ := tk.Token("call")
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("token %q is not URL path safe", tok)
	}
}
