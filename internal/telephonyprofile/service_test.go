package telephonyprofile

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CallHarborMCP/callharbor/internal/secrets"
	"github.com/CallHarborMCP/callharbor/internal/storage"
	"github.com/CallHarborMCP/callharbor/internal/telephony/telnyx"
)

const (
	testNow     = "2026-11-25T10:00:00.000Z"
	goodKey     = "KEY-good-0123456789" // gitleaks:allow
	rotatedKey  = "KEY-rotated-98765"   // gitleaks:allow
	testNumber  = "+14155550100"
	testAppID   = "app-7"
	otherAppID  = "app-8"
	testConnID  = testAppID
	fakePubKeyB = "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="
)

// memStore is an in-memory Store with the same owner-scoping rules as the
// SQL store, so service tests exercise the authorization logic here.
type memStore struct {
	profiles map[string]storage.TelephonyProfileRecord
	grants   []storage.TelephonyGrant
	users    map[string]storage.User
	secrets  map[string]storage.Secret
	audits   []storage.AuditEvent
	auditErr error             // when set, every audit write fails
	inUse    map[string]bool   // profile ids referenced by calls
	plain    map[string]string // test-only plaintext by secret id; the real store never holds this
	failNext string            // one-shot failure injection by method name
}

func newMemStore() *memStore {
	return &memStore{
		plain:    map[string]string{},
		profiles: map[string]storage.TelephonyProfileRecord{},
		users: map[string]storage.User{
			"alice": {ID: "alice", Role: "admin", Active: true},
			"bob":   {ID: "bob", Role: "user", Active: true},
			"carol": {ID: "carol", Role: "user", Active: true},
			"dave":  {ID: "dave", Role: "user", Active: false},
		},
		secrets: map[string]storage.Secret{},
		inUse:   map[string]bool{},
	}
}

func (m *memStore) fail(method string) error {
	if m.failNext == method {
		m.failNext = ""
		return errors.New("injected " + method)
	}
	return nil
}

func (m *memStore) CreateTelephonyProfile(_ context.Context, p storage.TelephonyProfileRecord) error {
	if err := m.fail("CreateTelephonyProfile"); err != nil {
		return err
	}
	for _, q := range m.profiles {
		if q.OwnerUserID == p.OwnerUserID && q.Name == p.Name {
			return storage.ErrConflict
		}
	}
	m.profiles[p.ID] = p
	return nil
}

func (m *memStore) GetTelephonyProfile(_ context.Context, owner, id string) (storage.TelephonyProfileRecord, error) {
	p, ok := m.profiles[id]
	if !ok || p.OwnerUserID != owner {
		return storage.TelephonyProfileRecord{}, storage.ErrTelephonyProfileNotFound
	}
	return p, nil
}

func (m *memStore) ListOwnedTelephonyProfiles(_ context.Context, owner string) ([]storage.TelephonyProfileRecord, error) {
	var out []storage.TelephonyProfileRecord
	for _, p := range m.profiles {
		if p.OwnerUserID == owner {
			out = append(out, p)
		}
	}
	return out, nil
}

// usableAccess applies the same rule as the SQL predicate in storage: an owner
// may use a profile while active; a grantee may use it while active and only
// while the owner is an active admin. It returns "" when the user may not.
func (m *memStore) usableAccess(p storage.TelephonyProfileRecord, user string) string {
	if p.OwnerUserID == user {
		if m.users[user].Active {
			return storage.TelephonyAccessOwner
		}
		return ""
	}
	owner := m.users[p.OwnerUserID]
	if m.hasGrant(p.ID, user) && m.users[user].Active && owner.Role == "admin" && owner.Active {
		return storage.TelephonyAccessGranted
	}
	return ""
}

func (m *memStore) ListUsableTelephonyProfiles(_ context.Context, user string) ([]storage.UsableTelephonyProfile, error) {
	var out []storage.UsableTelephonyProfile
	for _, p := range m.profiles {
		access := m.usableAccess(p, user)
		if access == "" {
			continue
		}
		out = append(out, usableRow(p, access))
	}
	return out, nil
}

func (m *memStore) ResolveUsableTelephonyProfile(_ context.Context, user, id string) (storage.UsableTelephonyProfile, error) {
	p, ok := m.profiles[id]
	if !ok {
		return storage.UsableTelephonyProfile{}, storage.ErrTelephonyProfileUnusable
	}
	access := m.usableAccess(p, user)
	if access == "" {
		return storage.UsableTelephonyProfile{}, storage.ErrTelephonyProfileUnusable
	}
	return usableRow(p, access), nil
}

func usableRow(p storage.TelephonyProfileRecord, access string) storage.UsableTelephonyProfile {
	return storage.UsableTelephonyProfile{TelephonyProfile: storage.TelephonyProfile{
		ID: p.ID, OwnerUserID: p.OwnerUserID, Name: p.Name, Provider: p.Provider,
		CallerNumber: p.CallerNumber, ConnectionID: p.ConnectionID,
		ConnectivityStatus: p.ConnectivityStatus, ConnectivityReason: p.ConnectivityReason,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}, Access: access}
}

func (m *memStore) hasGrant(profile, user string) bool {
	for _, g := range m.grants {
		if g.ProfileID == profile && g.GranteeUserID == user {
			return true
		}
	}
	return false
}

func (m *memStore) UpdateTelephonyProfile(_ context.Context, p storage.TelephonyProfileRecord) error {
	if err := m.fail("UpdateTelephonyProfile"); err != nil {
		return err
	}
	old, ok := m.profiles[p.ID]
	if !ok || old.OwnerUserID != p.OwnerUserID {
		return storage.ErrTelephonyProfileNotFound
	}
	for id, q := range m.profiles {
		if id != p.ID && q.OwnerUserID == p.OwnerUserID && q.Name == p.Name {
			return storage.ErrConflict
		}
	}
	if old.APISecretID != p.APISecretID {
		delete(m.secrets, old.APISecretID)
	}
	if old.WebhookPublicKeySecretID != "" && old.WebhookPublicKeySecretID != p.WebhookPublicKeySecretID {
		delete(m.secrets, old.WebhookPublicKeySecretID)
	}
	m.profiles[p.ID] = p
	return nil
}

func (m *memStore) DeleteTelephonyProfile(_ context.Context, owner, id string) error {
	if err := m.fail("DeleteTelephonyProfile"); err != nil {
		return err
	}
	p, ok := m.profiles[id]
	if !ok || p.OwnerUserID != owner {
		return storage.ErrTelephonyProfileNotFound
	}
	if m.inUse[id] {
		return storage.ErrTelephonyProfileInUse
	}
	delete(m.secrets, p.APISecretID)
	delete(m.secrets, p.WebhookPublicKeySecretID)
	delete(m.profiles, id)
	var kept []storage.TelephonyGrant
	for _, g := range m.grants {
		if g.ProfileID != id {
			kept = append(kept, g)
		}
	}
	m.grants = kept
	return nil
}

func (m *memStore) DeleteOwnedSecret(_ context.Context, owner, id string) error {
	if s, ok := m.secrets[id]; ok && s.OwnerUserID == owner {
		delete(m.secrets, id)
	}
	return nil
}

func (m *memStore) SetTelephonyConnectivity(_ context.Context, owner, id, status, reason, checkedAt string) error {
	if err := m.fail("SetTelephonyConnectivity"); err != nil {
		return err
	}
	p, ok := m.profiles[id]
	if !ok || p.OwnerUserID != owner {
		return storage.ErrTelephonyProfileNotFound
	}
	p.ConnectivityStatus = status
	p.ConnectivityReason = reason
	p.ConnectivityCheckedAt = checkedAt
	m.profiles[id] = p
	return nil
}

func (m *memStore) UpsertTelephonyGrant(_ context.Context, g storage.TelephonyGrant) error {
	if err := m.fail("UpsertTelephonyGrant"); err != nil {
		return err
	}
	if m.hasGrant(g.ProfileID, g.GranteeUserID) {
		return nil
	}
	m.grants = append(m.grants, g)
	return nil
}

func (m *memStore) DeleteTelephonyGrant(_ context.Context, profile, grantee string) error {
	for i, g := range m.grants {
		if g.ProfileID == profile && g.GranteeUserID == grantee {
			m.grants = append(m.grants[:i], m.grants[i+1:]...)
			return nil
		}
	}
	return storage.ErrTelephonyGrantNotFound
}

func (m *memStore) ListTelephonyGrants(_ context.Context, profile string) ([]storage.TelephonyGrant, error) {
	var out []storage.TelephonyGrant
	for _, g := range m.grants {
		if g.ProfileID == profile {
			out = append(out, g)
		}
	}
	return out, nil
}

func (m *memStore) GetUserByID(_ context.Context, id string) (storage.User, error) {
	u, ok := m.users[id]
	if !ok {
		return storage.User{}, storage.ErrNotFound
	}
	return u, nil
}

func (m *memStore) InsertAudit(_ context.Context, e storage.AuditEvent) error {
	if m.auditErr != nil {
		return m.auditErr
	}
	m.audits = append(m.audits, e)
	return nil
}

// memSecrets is an in-memory secret service with owner-scoped Resolve.
type memSecrets struct {
	store      *memStore
	noKey      bool
	invalid    bool // every Put returns ErrInvalidSecret
	n          int
	calls      int // Put calls so far
	failAt     int // the Put call number (1-based) that fails; 0 disables
	lastPlain  []string
	failResolv bool
}

func (s *memSecrets) Put(_ context.Context, owner, name, purpose string, plaintext []byte) (string, error) {
	s.calls++
	if s.failAt != 0 && s.calls == s.failAt {
		return "", errors.New("seal down")
	}
	if s.invalid {
		return "", secrets.ErrInvalidSecret
	}
	if s.noKey {
		return "", secrets.ErrNoMasterKey
	}
	if len(plaintext) == 0 || len(plaintext) > secrets.MaxPlaintext {
		return "", secrets.ErrInvalidSecret
	}
	s.n++
	id := fmt.Sprintf("sec-%d", s.n)
	s.store.secrets[id] = storage.Secret{ID: id, OwnerUserID: owner, Name: name, Purpose: purpose, Ciphertext: []byte("sealed")}
	s.store.plain[id] = string(plaintext)
	s.lastPlain = append(s.lastPlain, string(plaintext))
	return id, nil
}

func (s *memSecrets) Resolve(_ context.Context, owner, id, purpose string) ([]byte, error) {
	if s.noKey {
		return nil, secrets.ErrNoMasterKey
	}
	if s.failResolv {
		return nil, secrets.ErrDecrypt
	}
	row, ok := s.store.secrets[id]
	if !ok || row.OwnerUserID != owner || row.Purpose != purpose {
		return nil, secrets.ErrNotFound
	}
	return []byte(s.store.plain[id]), nil
}

type harness struct {
	t       *testing.T
	store   *memStore
	sec     *memSecrets
	fake    *telnyxREST
	svc     *Service
	srvURL  string
	idSeq   int
	factory ProviderFactory
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := newMemStore()
	h := &harness{t: t, store: store, fake: newTelnyxREST(goodKey)}
	srv := newFakeServer(t, h.fake)
	h.srvURL = srv.URL + "/v2"
	h.sec = &memSecrets{store: store}
	h.factory = func(apiKey string) (Provider, error) {
		c, err := telnyx.NewClient(h.srvURL, apiKey, srv.Client())
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	now := func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) }
	newID := func() string { h.idSeq++; return fmt.Sprintf("tp-%d", h.idSeq) }
	svc, err := New(store, h.sec, h.factory, now, newID, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

var (
	adminAlice = Actor{UserID: "alice", Admin: true}
	userBob    = Actor{UserID: "bob"}
	userCarol  = Actor{UserID: "carol"}
)

func validInput() Input {
	return Input{Name: "main", CallerNumber: testNumber, ConnectionID: testConnID, APIKey: goodKey}
}

// goodFake makes the fake healthy for the default number and application.
func (h *harness) goodFake() {
	h.fake.apps[testAppID] = appFixture{active: true}
	h.fake.numbers[testNumber] = numberFixture{id: "num-1", status: "active", connectionID: testConnID}
}

func (h *harness) create(actor Actor, in Input) View {
	h.t.Helper()
	v, err := h.svc.Create(context.Background(), actor, in)
	if err != nil {
		h.t.Fatalf("Create: %v", err)
	}
	return v
}

func TestCreateStoresEncryptedSecretsAndReturnsNoSecret(t *testing.T) {
	h := newHarness(t)
	in := validInput()
	in.WebhookPublicKey = fakePubKeyB
	v := h.create(adminAlice, in)
	if v.ConnectivityStatus != storage.ConnectivityUnknown {
		t.Fatalf("new profile status = %s, want unknown (creation never validates)", v.ConnectivityStatus)
	}
	if len(h.fake.recorded()) != 0 {
		t.Fatal("create contacted the provider")
	}
	rec := h.store.profiles[v.ID]
	if rec.APISecretID == "" || rec.WebhookPublicKeySecretID == "" {
		t.Fatalf("secret references missing: %+v", rec)
	}
	if h.store.secrets[rec.APISecretID].Purpose != secrets.PurposeTelnyxAPIKey {
		t.Fatalf("api key purpose = %s", h.store.secrets[rec.APISecretID].Purpose)
	}
	if h.store.secrets[rec.WebhookPublicKeySecretID].Purpose != secrets.PurposeTelnyxWebhookKey {
		t.Fatal("webhook key purpose wrong")
	}
	// The returned view must not carry any secret or secret id.
	raw := fmt.Sprintf("%+v", v)
	for _, forbidden := range []string{goodKey, rec.APISecretID, rec.WebhookPublicKeySecretID, fakePubKeyB} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("view leaked %q: %s", forbidden, raw)
		}
	}
}

func TestCreateRejectsInvalidInput(t *testing.T) {
	cases := map[string]func(in *Input){
		"empty name":         func(in *Input) { in.Name = "  " },
		"long name":          func(in *Input) { in.Name = strings.Repeat("n", 129) },
		"non e164":           func(in *Input) { in.CallerNumber = "4155550100" },
		"missing api key":    func(in *Input) { in.APIKey = "" },
		"huge api key":       func(in *Input) { in.APIKey = strings.Repeat("k", secrets.MaxPlaintext+1) },
		"bad public key":     func(in *Input) { in.WebhookPublicKey = "not-base64!" },
		"short public key":   func(in *Input) { in.WebhookPublicKey = base64.StdEncoding.EncodeToString([]byte("short")) },
		"long connection id": func(in *Input) { in.ConnectionID = strings.Repeat("c", 129) },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			in := validInput()
			mut(&in)
			_, err := h.svc.Create(context.Background(), adminAlice, in)
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err %v, want ErrInvalidInput", err)
			}
			if len(h.store.profiles) != 0 || len(h.store.secrets) != 0 {
				t.Fatal("invalid input left rows behind")
			}
		})
	}
}

func TestCreateWithoutMasterKeyFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.sec.noKey = true
	_, err := h.svc.Create(context.Background(), adminAlice, validInput())
	if !errors.Is(err, ErrSecretsDown) {
		t.Fatalf("err %v, want ErrSecretsDown", err)
	}
	if len(h.store.profiles) != 0 {
		t.Fatal("profile written without a key")
	}
}

func TestCreateRollsBackSecretsWhenProfileInsertFails(t *testing.T) {
	h := newHarness(t)
	in := validInput()
	in.WebhookPublicKey = fakePubKeyB
	h.store.failNext = "CreateTelephonyProfile"
	if _, err := h.svc.Create(context.Background(), adminAlice, in); err == nil {
		t.Fatal("insert failure was swallowed")
	}
	if len(h.store.secrets) != 0 {
		t.Fatalf("%d secrets orphaned after failed insert", len(h.store.secrets))
	}
}

func TestCreateDuplicateNameIsConflict(t *testing.T) {
	h := newHarness(t)
	h.create(adminAlice, validInput())
	_, err := h.svc.Create(context.Background(), adminAlice, validInput())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err %v, want ErrConflict", err)
	}
	if len(h.store.secrets) != 1 {
		t.Fatalf("duplicate create left %d secrets, want only the first", len(h.store.secrets))
	}
}

func TestUpdateResetsConnectivityAndReplacesKey(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	oldRef := h.store.profiles[v.ID].APISecretID
	if _, err := h.svc.Validate(context.Background(), adminAlice, v.ID); err != nil {
		t.Fatal(err)
	}
	in := validInput()
	in.APIKey = rotatedKey
	next, err := h.svc.Update(context.Background(), adminAlice, v.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if next.ConnectivityStatus != storage.ConnectivityUnknown {
		t.Fatalf("update kept status %s", next.ConnectivityStatus)
	}
	if _, ok := h.store.secrets[oldRef]; ok {
		t.Fatal("replaced key row survived")
	}
}

func TestUpdateKeepsSecretWhenKeyOmitted(t *testing.T) {
	h := newHarness(t)
	v := h.create(adminAlice, validInput())
	ref := h.store.profiles[v.ID].APISecretID
	in := validInput()
	in.APIKey = ""
	in.Name = "renamed"
	if _, err := h.svc.Update(context.Background(), adminAlice, v.ID, in); err != nil {
		t.Fatal(err)
	}
	if h.store.profiles[v.ID].APISecretID != ref {
		t.Fatal("omitted key changed the stored secret")
	}
}

func TestCrossUserWritesAreNotFound(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	ctx := context.Background()
	if _, err := h.svc.Update(ctx, userBob, v.ID, validInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob update = %v", err)
	}
	if err := h.svc.Delete(ctx, userBob, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob delete = %v", err)
	}
	if _, err := h.svc.Validate(ctx, userBob, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob validate = %v", err)
	}
	if _, err := h.svc.Get(ctx, userBob, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob get = %v", err)
	}
	if _, ok := h.store.profiles[v.ID]; !ok {
		t.Fatal("cross-user mutation removed the profile")
	}
	if n := len(h.fake.recorded()); n != 0 {
		t.Fatalf("bob's validate reached the provider %d times", n)
	}
}

func TestDeleteRemovesProfileSecretsAndGrants(t *testing.T) {
	h := newHarness(t)
	v := h.create(adminAlice, validInput())
	ctx := context.Background()
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Delete(ctx, adminAlice, v.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.store.secrets) != 0 || len(h.store.grants) != 0 {
		t.Fatalf("delete left secrets=%d grants=%d", len(h.store.secrets), len(h.store.grants))
	}
}

func TestDeleteInUseProfileIsRefused(t *testing.T) {
	h := newHarness(t)
	v := h.create(adminAlice, validInput())
	h.store.inUse[v.ID] = true
	if err := h.svc.Delete(context.Background(), adminAlice, v.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("err %v, want ErrInUse", err)
	}
	if _, ok := h.store.profiles[v.ID]; !ok {
		t.Fatal("in-use profile was deleted")
	}
}

func TestGetAndListShowOwnedAndGrantedOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	bob, err := h.svc.List(ctx, userBob)
	if err != nil || len(bob) != 1 || bob[0].Access != storage.TelephonyAccessGranted {
		t.Fatalf("bob list = %+v, %v", bob, err)
	}
	if _, err := h.svc.Get(ctx, userCarol, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted carol get = %v", err)
	}
	if list, _ := h.svc.List(ctx, userCarol); len(list) != 0 {
		t.Fatalf("carol sees %d profiles", len(list))
	}
}

func TestGrantRequiresAdminOwner(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	cases := map[string]Actor{
		"non-admin owner": {UserID: "alice", Admin: false},
		"other admin":     {UserID: "root", Admin: true},
		"plain user":      userBob,
	}
	for name, actor := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.svc.Grant(ctx, actor, v.ID, "carol"); err == nil {
				t.Fatal("grant allowed")
			}
			if err := h.svc.Revoke(ctx, actor, v.ID, "carol"); err == nil {
				t.Fatal("revoke allowed")
			}
			if _, err := h.svc.ListGrants(ctx, actor, v.ID); err == nil {
				t.Fatal("list grants allowed")
			}
		})
	}
	if len(h.store.grants) != 0 {
		t.Fatal("unauthorized grant wrote a row")
	}
}

func TestGrantLifecycleAndInactiveGranteeRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatalf("repeat grant should be idempotent: %v", err)
	}
	list, err := h.svc.ListGrants(ctx, adminAlice, v.ID)
	if err != nil || len(list) != 1 || list[0].GrantedBy != "alice" {
		t.Fatalf("grants = %+v, %v", list, err)
	}
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "dave"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("inactive grantee = %v", err)
	}
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "nobody"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown grantee = %v", err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Revoke(ctx, adminAlice, v.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke = %v", err)
	}
}

func TestGrantedUserCannotMutateOrValidate(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	ctx := context.Background()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Grant(ctx, adminAlice, v.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Update(ctx, userBob, v.ID, validInput()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("granted update = %v, want not found", err)
	}
	if err := h.svc.Delete(ctx, userBob, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("granted delete = %v", err)
	}
	if _, err := h.svc.Validate(ctx, userBob, v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("granted validate = %v", err)
	}
}

func TestValidateVerifiedWithGETOnly(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConnectivityStatus != storage.ConnectivityVerified || res.ConnectivityReason != "" {
		t.Fatalf("result = %+v", res)
	}
	if got := h.store.profiles[v.ID].ConnectivityStatus; got != storage.ConnectivityVerified {
		t.Fatalf("stored status = %s", got)
	}
	calls := h.fake.recorded()
	if len(calls) != 2 {
		t.Fatalf("requests = %+v, want application and number lookups", calls)
	}
	for _, c := range calls {
		if c.method != http.MethodGet {
			t.Fatalf("validation issued %s, only GET is allowed", c.method)
		}
	}
	if calls[0].path != "/v2/call_control_applications/"+testAppID {
		t.Fatalf("first request %s", calls[0].path)
	}
	if !strings.Contains(calls[1].query, "filter%5Bphone_number%5D=%2B14155550100") {
		t.Fatalf("number query %q", calls[1].query)
	}
}

func TestValidateReasonMatrix(t *testing.T) {
	type tc struct {
		name   string
		setup  func(h *harness)
		reason string
	}
	cases := []tc{
		{"wrong key", func(h *harness) {
			h.goodFake()
			h.fake.validKey = "someone-else" // gitleaks:allow
		}, ReasonInvalidCredentials},
		{"application missing", func(h *harness) {
			h.goodFake()
			delete(h.fake.apps, testAppID)
		}, ReasonApplicationNotFound},
		{"application inactive", func(h *harness) {
			h.goodFake()
			h.fake.apps[testAppID] = appFixture{active: false}
		}, ReasonApplicationInactive},
		{"number missing", func(h *harness) {
			h.goodFake()
			delete(h.fake.numbers, testNumber)
		}, ReasonNumberNotFound},
		{"number not active", func(h *harness) {
			h.goodFake()
			h.fake.numbers[testNumber] = numberFixture{id: "num-1", status: "port-pending", connectionID: testConnID}
		}, ReasonNumberInactive},
		{"number on other application", func(h *harness) {
			h.goodFake()
			h.fake.numbers[testNumber] = numberFixture{id: "num-1", status: "active", connectionID: otherAppID}
		}, ReasonNumberNotOnApp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h)
			v := h.create(adminAlice, validInput())
			res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if res.ConnectivityStatus != storage.ConnectivityFailed || res.ConnectivityReason != c.reason {
				t.Fatalf("result = %+v, want failed/%s", res, c.reason)
			}
			if got := h.store.profiles[v.ID].ConnectivityReason; got != c.reason {
				t.Fatalf("stored reason = %q", got)
			}
		})
	}
}

func TestValidateUnreachableProviderIsFailedNotHealthy(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	h.svc.provider = func(string) (Provider, error) {
		return unreachable{}, nil
	}
	res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConnectivityStatus != storage.ConnectivityFailed || res.ConnectivityReason != ReasonProviderUnreachable {
		t.Fatalf("result = %+v", res)
	}
}

func TestValidateSecretsUnavailableFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	h.sec.failResolv = true
	res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.ConnectivityReason != ReasonSecretsUnavailable || len(h.fake.recorded()) != 0 {
		t.Fatalf("result = %+v, provider calls = %d", res, len(h.fake.recorded()))
	}
}

func TestValidateRecordsNoProviderTextOrKey(t *testing.T) {
	h := newHarness(t)
	h.fake.validKey = "other" // gitleaks:allow
	v := h.create(adminAlice, validInput())
	res, err := h.svc.Validate(context.Background(), adminAlice, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("%+v %+v", res, h.store.profiles[v.ID])
	if strings.Contains(raw, goodKey) || strings.Contains(raw, "invalid api key") {
		t.Fatalf("validation leaked key or provider text: %s", raw)
	}
}

// unreachable is a provider whose every request fails at the transport.
type unreachable struct{}

func (unreachable) GetCallControlApplication(context.Context, string) (telnyx.CallControlApplication, error) {
	return telnyx.CallControlApplication{}, errors.New("transport down")
}

func (unreachable) FindPhoneNumber(context.Context, string) (telnyx.PhoneNumber, error) {
	return telnyx.PhoneNumber{}, errors.New("transport down")
}

func TestAuditRecordsActionsWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	h.goodFake()
	v := h.create(adminAlice, validInput())
	if _, err := h.svc.Validate(context.Background(), adminAlice, v.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.store.audits) < 2 {
		t.Fatalf("audits = %+v", h.store.audits)
	}
	for _, a := range h.store.audits {
		raw := fmt.Sprintf("%+v", a)
		if strings.Contains(raw, goodKey) || strings.Contains(raw, "sec-") {
			t.Fatalf("audit leaked secret material: %s", raw)
		}
	}
}

func TestNewRejectsMissingDependency(t *testing.T) {
	if _, err := New(nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("New accepted nil dependencies")
	}
}
