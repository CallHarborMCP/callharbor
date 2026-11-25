package mcp

import (
	"context"

	"github.com/CallHarborMCP/callharbor/internal/storage"
)

// fakeStore is an empty in-memory service store for registration tests.
type fakeStore struct{}

func (fakeStore) ListTelephonyProfiles(context.Context, string) ([]storage.TelephonyProfile, error) {
	return nil, nil
}

func (fakeStore) ListAIProfiles(context.Context, string) ([]storage.AIProfile, error) {
	return nil, nil
}

func (fakeStore) ListCalls(context.Context, string, string, *storage.CallCursor, int) ([]storage.Call, error) {
	return nil, nil
}

func (fakeStore) GetCall(context.Context, string, string) (storage.Call, error) {
	return storage.Call{}, storage.ErrCallNotFound
}
