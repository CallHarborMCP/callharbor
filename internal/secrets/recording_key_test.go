package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func TestDeriveRecordingKeyIsDeterministicPerSaltAndVersion(t *testing.T) {
	ring, err := NewKeyring(1, map[int][]byte{1: testKey(1), 2: testKey(9)})
	if err != nil {
		t.Fatal(err)
	}
	salt := bytes.Repeat([]byte{7}, RecordingSaltSize)
	other := bytes.Repeat([]byte{8}, RecordingSaltSize)

	a, err := ring.DeriveRecordingKey(1, salt)
	if err != nil || len(a) != KeySize {
		t.Fatalf("derive: len=%d err=%v", len(a), err)
	}
	again, _ := ring.DeriveRecordingKey(1, salt)
	if !bytes.Equal(a, again) {
		t.Fatal("same master, version and salt must derive the same key")
	}
	b, _ := ring.DeriveRecordingKey(1, other)
	if bytes.Equal(a, b) {
		t.Fatal("different salts must derive different keys")
	}
	c, _ := ring.DeriveRecordingKey(2, salt)
	if bytes.Equal(a, c) {
		t.Fatal("different key versions must derive different keys")
	}
}

func TestDeriveRecordingKeyRejectsBadInputs(t *testing.T) {
	ring, err := NewKeyring(1, map[int][]byte{1: testKey(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.DeriveRecordingKey(3, make([]byte, RecordingSaltSize)); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("unknown version: err = %v, want ErrUnknownKeyVersion", err)
	}
	if _, err := ring.DeriveRecordingKey(1, make([]byte, 16)); !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("short salt: err = %v, want ErrInvalidSecret", err)
	}
	if _, err := ring.DeriveRecordingKey(1, nil); !errors.Is(err, ErrInvalidSecret) {
		t.Fatalf("nil salt: err = %v, want ErrInvalidSecret", err)
	}
}
