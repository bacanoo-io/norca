// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package prometheus

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/norca/autoscaler/sdk"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
)

// The error of a query that can't reach Prometheus becomes the message of
// the evaluation in the status of a scaling policy, which the readers of the
// job see: it holds no credential of the address of the server
// configuration.
func TestAPMPlugin_Query_ErrorHidesAddressCredentials(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	u, err := url.Parse(dead.URL)
	must.NoError(t, err)
	dead.Close()

	now := time.Now()
	for name, set := range map[string]func(){
		"user and password": func() { u.User = url.UserPassword("SECRETTOKEN", "SECRETPASS") },
		"token as user":     func() { u.User = url.User("SECRETTOKEN") },
		"query value":       func() { u.User = nil; u.RawQuery = "api_key=SECRETVALUE" },
	} {
		t.Run(name, func(t *testing.T) {
			set()
			plugin := NewPrometheusPlugin(hclog.NewNullLogger())
			must.NoError(t, plugin.SetConfig(map[string]string{configKeyAddress: u.String()}))

			for _, r := range []sdk.TimeRange{{From: now, To: now}, {From: now.Add(-time.Minute), To: now}} {
				_, err := plugin.Query("up", r)
				must.Error(t, err)
				must.StrContains(t, err.Error(), "failed to query")
				must.StrNotContains(t, err.Error(), "SECRET")
			}
		})
	}

	// an address that can't be parsed is not echoed either
	plugin := NewPrometheusPlugin(hclog.NewNullLogger())
	err = plugin.SetConfig(map[string]string{configKeyAddress: "https://SECRETTOKEN@prom.example.com/\x7f"})
	must.Error(t, err)
	must.StrNotContains(t, err.Error(), "SECRET")
}
