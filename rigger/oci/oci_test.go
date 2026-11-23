// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package oci

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A repository is a host and a name. One written with a user and a
// password is refused before a request, and the error does not repeat them.
func TestRef_Credentials(t *testing.T) {
	t.Parallel()

	requests := 0

	c := testClient(func(*http.Request) *http.Response {
		requests++

		return answer(http.StatusOK, `{"tags":[]}`)
	})

	_, err := c.Tags(t.Context(), "bob:s3cret@"+testRegistry+"/acme/web")
	if !errors.Is(err, ErrRegistry) {
		t.Errorf("Tags() = %v, want ErrRegistry", err)
	}

	if err != nil && strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error repeats the password: %v", err)
	}

	if requests != 0 {
		t.Errorf("%d requests were sent", requests)
	}
}
