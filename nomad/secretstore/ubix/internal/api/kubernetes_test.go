package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeTokenReview returns an httptest server that answers TokenReview requests
// with the given authenticated username.
func fakeTokenReview(t *testing.T, username string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":{"authenticated":true,"user":{"username":"` + username + `"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// configureK8s sets up the Kubernetes auth method against a fake apiserver and
// writes a role. Returns the handler and root token.
func configureK8s(t *testing.T, boundNamespace string) (http.Handler, string) {
	t.Helper()
	h, root := unsealedHandler(t)
	api := fakeTokenReview(t, "system:serviceaccount:team-a:app-sa")

	if rec := doAuth(t, h, "POST", "/v1/auth/kubernetes/config",
		`{"kubernetes_host":"`+api.URL+`"}`, root); rec.Code != http.StatusNoContent {
		t.Fatalf("k8s config = %d, body=%s", rec.Code, rec.Body.String())
	}
	role := `{"bound_service_account_namespaces":["` + boundNamespace + `"],"bound_service_account_names":["app-sa"],"policies":["app-ro"]}`
	if rec := doAuth(t, h, "POST", "/v1/auth/kubernetes/role/app", role, root); rec.Code != http.StatusNoContent {
		t.Fatalf("k8s role = %d, body=%s", rec.Code, rec.Body.String())
	}
	return h, root
}

func TestK8sLoginSuccess(t *testing.T) {
	h, _ := configureK8s(t, "team-a")

	// Login is unauthenticated (no X-Vault-Token).
	rec := do(t, h, "POST", "/v1/auth/kubernetes/login", `{"role":"app","jwt":"a.sa.token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, body=%s", rec.Code, rec.Body.String())
	}
	auth := decode[map[string]any](t, rec)["auth"].(map[string]any)
	if auth["client_token"] == "" {
		t.Fatalf("login returned no client_token: %v", auth)
	}
	if pols := auth["policies"].([]any); len(pols) != 1 || pols[0] != "app-ro" {
		t.Fatalf("login policies = %v, want [app-ro]", pols)
	}
}

func TestK8sLoginDeniedWrongNamespace(t *testing.T) {
	// The fake apiserver says team-a, but the role only binds team-b.
	h, _ := configureK8s(t, "team-b")
	rec := do(t, h, "POST", "/v1/auth/kubernetes/login", `{"role":"app","jwt":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("login wrong namespace = %d, want 403", rec.Code)
	}
}

func TestK8sLoginUnknownRole(t *testing.T) {
	h, _ := configureK8s(t, "team-a")
	rec := do(t, h, "POST", "/v1/auth/kubernetes/login", `{"role":"nope","jwt":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown role = %d, want 404", rec.Code)
	}
}

func TestK8sConfigRequiresAuth(t *testing.T) {
	h, _ := unsealedHandler(t)
	// Config is an admin operation — no token must be rejected.
	if rec := do(t, h, "POST", "/v1/auth/kubernetes/config", `{"kubernetes_host":"https://x"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("config without token = %d, want 401", rec.Code)
	}
}

func TestK8sIssuedTokenWorks(t *testing.T) {
	h, root := configureK8s(t, "team-a")
	// Seed a secret and grant the app-ro policy read on it.
	doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"pw":"s3cr3t"}}`, root)
	doAuth(t, h, "PUT", "/v1/sys/policies/acl/app-ro",
		`{"path":{"secret/data/app/db":{"capabilities":["read"]}}}`, root)

	// Log in as the pod and use the issued token.
	rec := do(t, h, "POST", "/v1/auth/kubernetes/login", `{"role":"app","jwt":"x"}`)
	podToken := decode[map[string]any](t, rec)["auth"].(map[string]any)["client_token"].(string)

	if rec := doAuth(t, h, "GET", "/v1/secret/data/app/db", "", podToken); rec.Code != http.StatusOK {
		t.Fatalf("pod token read = %d, want 200", rec.Code)
	}
	// And it cannot write (policy is read-only).
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app/db", `{"data":{"x":"y"}}`, podToken); rec.Code != http.StatusForbidden {
		t.Fatalf("pod token write = %d, want 403", rec.Code)
	}
}
