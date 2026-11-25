package mcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// fakeStore is an in-memory service store. It keeps owner scoping in the same
// shape as the real store, so isolation tests exercise the service's Actor
// handling and not a fixed fixture. Failures can be injected per method.
type fakeStore struct {
	mu        sync.Mutex
	telephony []storage.TelephonyProfile
	ai        []storage.AIProfile
	calls     []storage.Call

	telErr  error
	aiErr   error
	listErr error
	getErr  error
}

func (f *fakeStore) ListUsableTelephonyProfiles(_ context.Context, owner string) ([]storage.UsableTelephonyProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.telErr != nil {
		return nil, f.telErr
	}
	var out []storage.UsableTelephonyProfile
	for _, p := range f.telephony {
		if p.OwnerUserID == owner {
			if p.ConnectivityStatus == "" {
				p.ConnectivityStatus = "unknown" // the stored default
			}
			out = append(out, storage.UsableTelephonyProfile{TelephonyProfile: p, Access: "owner"})
		}
	}
	return out, nil
}

func (f *fakeStore) ListAIProfiles(_ context.Context, owner string) ([]storage.AIProfile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.aiErr != nil {
		return nil, f.aiErr
	}
	var out []storage.AIProfile
	for _, p := range f.ai {
		if p.OwnerUserID == owner {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeStore) ListCalls(_ context.Context, owner, status string, after *storage.CallCursor, limit int) ([]storage.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []storage.Call
	for _, c := range f.calls {
		if c.OwnerUserID != owner || (status != "" && c.Status != status) {
			continue
		}
		if after != nil && (c.CreatedAt > after.CreatedAt || (c.CreatedAt == after.CreatedAt && c.ID >= after.ID)) {
			continue
		}
		out = append(out, c)
	}
	// Newest first, the order the storage layer returns.
	sortCalls(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) GetCall(_ context.Context, owner, id string) (storage.Call, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return storage.Call{}, f.getErr
	}
	for _, c := range f.calls {
		if c.ID == id && c.OwnerUserID == owner {
			return c, nil
		}
	}
	return storage.Call{}, storage.ErrCallNotFound
}

// sortCalls orders newest first by (created_at, id), matching the keyset.
func sortCalls(cs []storage.Call) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && lessCall(cs[j-1], cs[j]); j-- {
			cs[j-1], cs[j] = cs[j], cs[j-1]
		}
	}
}

func lessCall(a, b storage.Call) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.ID < b.ID
}

// syncBuffer is a log sink safe for concurrent writes from the handler.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger returns a logger whose output is kept for assertions.
func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// errInjected is a store failure whose text must never reach a client.
var errInjected = errors.New("pq: secret connection string user=svc password=hunter2")
