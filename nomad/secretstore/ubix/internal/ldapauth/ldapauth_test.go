package ldapauth

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// fakeConnector authenticates one fixed credential and returns fixed groups.
type fakeConnector struct {
	username string
	password string
	groups   []string
	err      error // if set, Authenticate returns it (connection failure)
}

func (f fakeConnector) Authenticate(_ context.Context, _ *Config, username, password string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if username != f.username || password != f.password {
		return nil, ErrDenied
	}
	return f.groups, nil
}

func newMethod(t *testing.T, conn Connector) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	return NewWithConnector(mem, token.NewStore(mem), "auth/ldap", conn)
}

func validConfig() Config {
	return Config{URL: "ldap://dir.test:389", UserDN: "ou=people,dc=test", GroupDN: "ou=groups,dc=test"}
}

func TestConfigureValidation(t *testing.T) {
	m := newMethod(t, fakeConnector{})
	if err := m.Configure(context.Background(), Config{URL: "ldap://x"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("configure without user_dn = %v, want ErrInvalidConfig", err)
	}
	if err := m.Configure(context.Background(), Config{UserDN: "ou=people"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("configure without url = %v, want ErrInvalidConfig", err)
	}
	if err := m.Configure(context.Background(), validConfig()); err != nil {
		t.Fatalf("valid configure: %v", err)
	}
}

func TestLoginNotConfigured(t *testing.T) {
	m := newMethod(t, fakeConnector{})
	if _, err := m.Login(context.Background(), "alice", "pw"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("login before configure = %v, want ErrNotConfigured", err)
	}
}

func TestLoginBadCredentials(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t, fakeConnector{username: "alice", password: "right", groups: nil})
	if err := m.Configure(ctx, validConfig()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := m.Login(ctx, "alice", "wrong"); !errors.Is(err, ErrDenied) {
		t.Fatalf("bad password = %v, want ErrDenied", err)
	}
	// An empty password must be rejected before the directory is contacted.
	if _, err := m.Login(ctx, "alice", ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("empty password = %v, want ErrDenied", err)
	}
}

func TestLoginUnionsGroupPolicies(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t, fakeConnector{username: "alice", password: "pw", groups: []string{"platform", "eng", "unmapped"}})
	if err := m.Configure(ctx, validConfig()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := m.WriteGroup(ctx, "platform", []string{"kv-admin", "shared"}); err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}
	if err := m.WriteGroup(ctx, "eng", []string{"shared", "ci"}); err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}

	tok, err := m.Login(ctx, "alice", "pw")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	got := append([]string(nil), tok.Policies...)
	sort.Strings(got)
	want := []string{"ci", "kv-admin", "shared"} // union, deduped; "unmapped" contributes nothing
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policies = %v, want %v", got, want)
	}
}

func TestGroupCRUD(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t, fakeConnector{})
	if err := m.WriteGroup(ctx, "team", []string{"p1"}); err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}
	got, err := m.ReadGroup(ctx, "team")
	if err != nil || len(got) != 1 || got[0] != "p1" {
		t.Fatalf("ReadGroup = %v, %v", got, err)
	}
	names, _ := m.ListGroups(ctx)
	if len(names) != 1 || names[0] != "team" {
		t.Fatalf("ListGroups = %v", names)
	}
	if err := m.DeleteGroup(ctx, "team"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if got, _ := m.ReadGroup(ctx, "team"); got != nil {
		t.Fatalf("after delete ReadGroup = %v, want nil", got)
	}
	if err := m.WriteGroup(ctx, "bad/name", nil); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("WriteGroup(bad/name) = %v, want ErrInvalidName", err)
	}
}

func TestLoginConnectionErrorPropagates(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("dial tcp: connection refused")
	m := newMethod(t, fakeConnector{err: boom})
	if err := m.Configure(ctx, validConfig()); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := m.Login(ctx, "alice", "pw"); !errors.Is(err, boom) {
		t.Fatalf("connection error = %v, want the dial error", err)
	}
}
