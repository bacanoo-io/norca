package kubeauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/token"
)

// mockReviewer returns a fixed result, standing in for the TokenReview API.
type mockReviewer struct {
	result *ReviewResult
	err    error
}

func (m *mockReviewer) Review(context.Context, string) (*ReviewResult, error) {
	return m.result, m.err
}

// newMethod returns a method whose reviewer is the given mock.
func newMethod(t *testing.T, rev TokenReviewer) *Method {
	t.Helper()
	mem := storage.NewMemoryBackend()
	m := New(mem, token.NewStore(mem), "auth/kubernetes")
	m.newReviewer = func(Config) (TokenReviewer, error) { return rev, nil }
	return m
}

func configuredMethod(t *testing.T, rev TokenReviewer) *Method {
	t.Helper()
	m := newMethod(t, rev)
	if err := m.Configure(context.Background(), Config{Host: "https://k8s.example"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	return m
}

func TestLoginSuccess(t *testing.T) {
	ctx := context.Background()
	rev := &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "team-a", ServiceAccount: "app-sa"}}
	m := configuredMethod(t, rev)

	role := Role{
		BoundServiceAccountNamespaces: []string{"team-a"},
		BoundServiceAccountNames:      []string{"app-sa"},
		Policies:                      []string{"app-ro"},
	}
	if err := m.WriteRole(ctx, "app", role); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	tok, err := m.Login(ctx, "app", "a.jwt.token")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !reflect.DeepEqual(tok.Policies, []string{"app-ro"}) {
		t.Fatalf("issued token policies = %v, want [app-ro]", tok.Policies)
	}
}

// TestResetRebuildsReviewer: after Reset (a seal) the next login builds the
// TokenReview client again from the stored config.
func TestResetRebuildsReviewer(t *testing.T) {
	ctx := context.Background()
	rev := &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "ns", ServiceAccount: "sa"}}
	m := configuredMethod(t, rev)
	builds := 0
	m.newReviewer = func(Config) (TokenReviewer, error) { builds++; return rev, nil }
	if err := m.WriteRole(ctx, "app", Role{
		BoundServiceAccountNamespaces: []string{"ns"}, BoundServiceAccountNames: []string{"sa"},
	}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	if _, err := m.Login(ctx, "app", "jwt"); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if builds != 0 {
		t.Fatalf("reviewer rebuilt %d times before Reset, want 0 (cached from Configure)", builds)
	}
	m.Reset()
	if _, err := m.Login(ctx, "app", "jwt"); err != nil {
		t.Fatalf("Login after Reset: %v", err)
	}
	if builds != 1 {
		t.Fatalf("reviewer built %d times after Reset, want 1", builds)
	}
}

func TestLoginWrongNamespaceOrName(t *testing.T) {
	ctx := context.Background()
	m := configuredMethod(t, &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "team-b", ServiceAccount: "app-sa"}})
	_ = m.WriteRole(ctx, "app", Role{
		BoundServiceAccountNamespaces: []string{"team-a"},
		BoundServiceAccountNames:      []string{"app-sa"},
		Policies:                      []string{"app-ro"},
	})
	if _, err := m.Login(ctx, "app", "jwt"); !errors.Is(err, ErrDenied) {
		t.Fatalf("wrong namespace: want ErrDenied, got %v", err)
	}
}

func TestLoginUnauthenticated(t *testing.T) {
	ctx := context.Background()
	m := configuredMethod(t, &mockReviewer{result: &ReviewResult{Authenticated: false}})
	_ = m.WriteRole(ctx, "app", Role{BoundServiceAccountNamespaces: []string{"*"}, BoundServiceAccountNames: []string{"*"}})
	if _, err := m.Login(ctx, "app", "bad-jwt"); !errors.Is(err, ErrDenied) {
		t.Fatalf("unauthenticated: want ErrDenied, got %v", err)
	}
}

func TestLoginWildcardBindings(t *testing.T) {
	ctx := context.Background()
	m := configuredMethod(t, &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "any-ns", ServiceAccount: "any-sa"}})
	_ = m.WriteRole(ctx, "wild", Role{
		BoundServiceAccountNamespaces: []string{"*"},
		BoundServiceAccountNames:      []string{"*"},
		Policies:                      []string{"p"},
	})
	if _, err := m.Login(ctx, "wild", "jwt"); err != nil {
		t.Fatalf("wildcard login: %v", err)
	}
}

func TestLoginUnknownRole(t *testing.T) {
	ctx := context.Background()
	m := configuredMethod(t, &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "n", ServiceAccount: "s"}})
	if _, err := m.Login(ctx, "missing", "jwt"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("want ErrRoleNotFound, got %v", err)
	}
}

func TestLoginBeforeConfigure(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t, &mockReviewer{})
	if _, err := m.Login(ctx, "app", "jwt"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

func TestRoleCRUD(t *testing.T) {
	ctx := context.Background()
	m := newMethod(t, &mockReviewer{})
	role := Role{BoundServiceAccountNamespaces: []string{"team-a"}, Policies: []string{"p"}}
	if err := m.WriteRole(ctx, "app", role); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	if got, err := m.ReadRole(ctx, "app"); err != nil || got.Policies[0] != "p" {
		t.Fatalf("ReadRole = %+v, err %v", got, err)
	}
	if names, _ := m.ListRoles(ctx); len(names) != 1 || names[0] != "app" {
		t.Fatalf("ListRoles = %v", names)
	}
	if err := m.DeleteRole(ctx, "app"); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if _, err := m.ReadRole(ctx, "app"); !errors.Is(err, ErrRoleNotFound) {
		t.Fatalf("after delete: want ErrRoleNotFound, got %v", err)
	}
}

func TestInvalidRoleName(t *testing.T) {
	m := newMethod(t, &mockReviewer{})
	if err := m.WriteRole(context.Background(), "bad/name", Role{}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("want ErrInvalidName, got %v", err)
	}
}

// --- HTTP reviewer against a fake TokenReview endpoint ---

func fakeAPIServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestHTTPReviewerAuthenticated(t *testing.T) {
	srv := fakeAPIServer(t, 201, `{"status":{"authenticated":true,"user":{"username":"system:serviceaccount:team-a:app-sa"}}}`)
	rev, err := newHTTPReviewer(Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("newHTTPReviewer: %v", err)
	}
	res, err := rev.Review(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if !res.Authenticated || res.Namespace != "team-a" || res.ServiceAccount != "app-sa" {
		t.Fatalf("review result = %+v", res)
	}
}

func TestHTTPReviewerRejected(t *testing.T) {
	srv := fakeAPIServer(t, 201, `{"status":{"authenticated":false,"error":"invalid token"}}`)
	rev, _ := newHTTPReviewer(Config{Host: srv.URL})
	res, err := rev.Review(context.Background(), "jwt")
	if err != nil || res.Authenticated {
		t.Fatalf("expected unauthenticated, got %+v err %v", res, err)
	}
}

func TestHTTPReviewerNonServiceAccountIsUnbindable(t *testing.T) {
	srv := fakeAPIServer(t, 201, `{"status":{"authenticated":true,"user":{"username":"kubernetes-admin"}}}`)
	rev, _ := newHTTPReviewer(Config{Host: srv.URL})
	res, _ := rev.Review(context.Background(), "jwt")
	if res.Authenticated {
		t.Fatalf("non-ServiceAccount identity should not be bindable: %+v", res)
	}
}

func TestHTTPReviewerServerError(t *testing.T) {
	srv := fakeAPIServer(t, 500, `{}`)
	rev, _ := newHTTPReviewer(Config{Host: srv.URL})
	if _, err := rev.Review(context.Background(), "jwt"); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestParseServiceAccount(t *testing.T) {
	cases := map[string][3]string{
		"system:serviceaccount:team-a:app-sa": {"team-a", "app-sa", "ok"},
		"system:serviceaccount:default:build": {"default", "build", "ok"},
		"kubernetes-admin":                    {"", "", ""},
		"system:serviceaccount:onlyns":        {"", "", ""},
	}
	for in, want := range cases {
		ns, sa, ok := parseServiceAccount(in)
		gotOK := ""
		if ok {
			gotOK = "ok"
		}
		if ns != want[0] || sa != want[1] || gotOK != want[2] {
			t.Errorf("parseServiceAccount(%q) = (%q,%q,%v)", in, ns, sa, ok)
		}
	}
}

// The role's ttl bounds the issued token (it used to be stored but ignored, so
// every login got the 32-day default).
func TestLoginAppliesRoleTTL(t *testing.T) {
	ctx := context.Background()
	rev := &mockReviewer{result: &ReviewResult{Authenticated: true, Namespace: "team-a", ServiceAccount: "app-sa"}}
	m := configuredMethod(t, rev)
	for name, ttl := range map[string]time.Duration{"short": 10 * time.Minute, "default": 0} {
		if err := m.WriteRole(ctx, name, Role{
			BoundServiceAccountNamespaces: []string{"team-a"},
			BoundServiceAccountNames:      []string{"app-sa"},
			Policies:                      []string{"app-ro"},
			TTL:                           ttl,
		}); err != nil {
			t.Fatalf("WriteRole %s: %v", name, err)
		}
	}

	short, err := m.Login(ctx, "short", "a.jwt.token")
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(short.ExpiresAt); left > 10*time.Minute || left < 9*time.Minute {
		t.Fatalf("role ttl 10m: token expires in %v", left)
	}
	def, err := m.Login(ctx, "default", "a.jwt.token")
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(def.ExpiresAt); left < token.DefaultTTL-time.Minute {
		t.Fatalf("no role ttl: token expires in %v, want the default %v", left, token.DefaultTTL)
	}
}
