// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"net/http"
	"strings"
	"testing"
)

// The credentials come from the host of the client, asked by the host of
// the registry: never from the environment, and never for another
// registry.
func TestClient_CredentialsOfAHost(t *testing.T) {
	t.Setenv("RIGGER_OCI_USERNAME", "env")
	t.Setenv("RIGGER_OCI_PASSWORD", "env-fixture")

	const other = "other.example.net"

	var seen []string

	c := testClient(func(r *http.Request) *http.Response {
		if user, password, ok := r.BasicAuth(); ok {
			seen = append(seen, r.URL.Host+"="+user+":"+password)

			return answer(http.StatusOK, `{"tags":[]}`)
		}

		return answer(http.StatusUnauthorized, "", "WWW-Authenticate", "Basic")
	})

	var asked []string

	c.SetCredentials(func(host string) (string, string, bool, error) {
		asked = append(asked, host)
		if host == testRegistry {
			return "robot", "fixture", true, nil
		}

		return "", "", false, nil
	})

	if _, err := c.Tags(t.Context(), testRegistry+"/acme/web"); err != nil {
		t.Errorf("Tags() of the registry = %v", err)
	}

	if _, err := c.Tags(t.Context(), other+"/acme/web"); err == nil {
		t.Error("another registry got credentials")
	}

	if got := strings.Join(seen, " "); got != testRegistry+"=robot:fixture" {
		t.Errorf("credentials seen: %q", got)
	}

	if got := strings.Join(asked, " "); got != testRegistry+" "+other {
		t.Errorf("hosts asked: %q", got)
	}
}
