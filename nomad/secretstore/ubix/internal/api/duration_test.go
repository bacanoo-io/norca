package api

import (
	"encoding/json"
	"testing"
)

func TestVaultDurationForms(t *testing.T) {
	cases := map[string]vaultDuration{
		`{"d":"1h"}`:   "1h",
		`{"d":"90m"}`:  "90m",
		`{"d":"3600"}`: "3600s",
		`{"d":3600}`:   "3600s",
		`{"d":0}`:      "0s",
		`{"d":""}`:     "",
		`{"d":null}`:   "",
		`{}`:           "",
		`{"d":"soon"}`: "soon", // left for time.ParseDuration to reject, as before
	}
	for in, want := range cases {
		var v struct {
			D vaultDuration `json:"d"`
		}
		if err := json.Unmarshal([]byte(in), &v); err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if v.D != want {
			t.Errorf("%s = %q, want %q", in, v.D, want)
		}
	}
	for _, bad := range []string{`{"d":1.5}`, `{"d":true}`, `{"d":[1]}`} {
		var v struct {
			D vaultDuration `json:"d"`
		}
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("%s: want an error, got %q", bad, v.D)
		}
	}
}

// Vault's client renews with a number of seconds.
func TestRenewSelfNumericIncrement(t *testing.T) {
	h, root := unsealedHandler(t)
	tok := createPlainToken(t, h, root)
	rec := doAuth(t, h, "PUT", "/v1/auth/token/renew-self", `{"increment":7200}`, tok)
	if rec.Code != 200 {
		t.Fatalf("renew-self with numeric increment = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ld := decode[map[string]any](t, rec)["auth"].(map[string]any)["lease_duration"].(float64); ld < 7100 || ld > 7200 {
		t.Fatalf("lease_duration = %v, want ~7200", ld)
	}
}
