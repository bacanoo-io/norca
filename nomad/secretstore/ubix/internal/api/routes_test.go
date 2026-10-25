package api

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// writeRoutePaths returns every path registered through handleWrite in sys.go.
func writeRoutePaths(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("sys.go")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, m := range regexp.MustCompile(`handleWrite\(mux, "(/v1/[^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		paths = append(paths, m[1])
	}
	if len(paths) < 50 {
		t.Fatalf("found only %d handleWrite routes; the pattern is out of date", len(paths))
	}
	return paths
}

// Write routes must be registered through handleWrite, never as a bare POST or
// PUT: Vault treats the two as one, and clients split between them.
func TestWriteRoutesUseHandleWrite(t *testing.T) {
	src, err := os.ReadFile("sys.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("(POST|PUT) (/v1/[^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		t.Errorf("%s %s is registered for one method only; use handleWrite", m[1], m[2])
	}
}

// Every write route answers both PUT and POST — whatever the status (401, 400,
// 503...), it must never be 405 Method Not Allowed.
func TestWriteRoutesAcceptPutAndPost(t *testing.T) {
	h, _ := unsealedHandler(t)
	fill := strings.NewReplacer("{path...}", "a/b", "{mode}", "plaintext")
	param := regexp.MustCompile(`\{[a-z_]+\}`)
	for _, p := range writeRoutePaths(t) {
		path := param.ReplaceAllString(fill.Replace(p), "x")
		for _, method := range []string{http.MethodPut, http.MethodPost} {
			if rec := do(t, h, method, path, `{}`); rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("%s %s = 405", method, path)
			}
		}
	}
}

// The clients this is for: the Vault CLI initializes and unseals with PUT.
func TestInitAndUnsealWithPut(t *testing.T) {
	h := newTestHandler()
	rec := do(t, h, http.MethodPut, "/v1/sys/init", `{"secret_shares":3,"secret_threshold":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT sys/init = %d, body=%s", rec.Code, rec.Body.String())
	}
	keys := decode[map[string]any](t, rec)["keys"].([]any)
	for _, k := range keys[:2] {
		rec = do(t, h, http.MethodPut, "/v1/sys/unseal", `{"key":"`+k.(string)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT sys/unseal = %d, body=%s", rec.Code, rec.Body.String())
		}
	}
	if sealed := decode[map[string]any](t, rec)["sealed"]; sealed != false {
		t.Fatalf("after two PUT unseals, sealed = %v", sealed)
	}
}

// Vault's client lists with GET ?list=true; it must behave exactly like LIST,
// including at the top of the KV store and under the list ACL capability.
func TestGetListTrueIsList(t *testing.T) {
	h, root := unsealedHandler(t)
	if rec := doAuth(t, h, "POST", "/v1/secret/data/app/one", `{"data":{"k":"v"}}`, root); rec.Code != http.StatusOK {
		t.Fatalf("write = %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/v1/secret/metadata/?list=true", "/v1/secret/metadata/app?list=true"} {
		rec := doAuth(t, h, "GET", path, "", root)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body=%s", path, rec.Code, rec.Body.String())
		}
		if keys := decode[map[string]any](t, rec)["data"].(map[string]any)["keys"].([]any); len(keys) != 1 {
			t.Fatalf("GET %s keys = %v, want one entry", path, keys)
		}
	}

	// A token that may read but not list is refused a GET ?list=true.
	setPolicy(t, h, root, "readonly", `{"path":{"secret/*":{"capabilities":["read"]}}}`)
	tok := tokenWith(t, h, root, `{"policies":["readonly"],"ttl":"1h"}`)
	if rec := doAuth(t, h, "GET", "/v1/secret/metadata/?list=true", "", tok); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token GET ?list=true = %d, want 403", rec.Code)
	}
	// ...while a plain GET (a read) is still a read.
	if rec := doAuth(t, h, "GET", "/v1/secret/data/app/one", "", tok); rec.Code != http.StatusOK {
		t.Fatalf("read-only token read = %d, want 200", rec.Code)
	}
}

// Vault's client sends display_name, num_uses, type and entity_alias on every
// token create. Zero values must pass; values that would change what the token
// may do must be refused, never silently ignored.
func TestTokenCreateVaultClientFields(t *testing.T) {
	h, root := unsealedHandler(t)
	ok := `{"policies":["p"],"ttl":"1h","display_name":"","num_uses":0,"type":"","entity_alias":""}`
	if rec := doAuth(t, h, "PUT", "/v1/auth/token/create", ok, root); rec.Code != http.StatusOK {
		t.Fatalf("zero-valued client fields = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(t, h, "PUT", "/v1/auth/token/create", `{"policies":["p"],"display_name":"ci","type":"service"}`, root); rec.Code != http.StatusOK {
		t.Fatalf("display_name + type service = %d, body=%s", rec.Code, rec.Body.String())
	}
	for _, bad := range []string{
		`{"policies":["p"],"num_uses":1}`,
		`{"policies":["p"],"type":"batch"}`,
		`{"policies":["p"],"entity_alias":"someone"}`,
	} {
		if rec := doAuth(t, h, "PUT", "/v1/auth/token/create", bad, root); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
}
