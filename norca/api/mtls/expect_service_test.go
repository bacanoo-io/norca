// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtls

import (
	"testing"

	"github.com/shoenig/test/must"
)

func TestExpectService_CaseInsensitive(t *testing.T) {
	peer := &Peer{ID: ID{TrustDomain: "a.nomad", Namespace: "prod", Job: "api", Group: "be"}, Services: []string{"myapi"}}
	// The services of a certificate are lower case, the name of a service
	// need not be.
	must.NoError(t, ExpectService("prod", "MyAPI")(peer))
	must.NoError(t, ExpectService("prod", "myapi")(peer))
	must.ErrorContains(t, ExpectService("prod", "other")(peer), `is not service "other"`)
	must.ErrorContains(t, ExpectService("Prod", "myapi")(peer), "is not in namespace")
}
