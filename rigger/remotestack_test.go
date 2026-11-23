// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rigger_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger"
)

// An address with credentials is refused, and neither the diagnostic nor
// the log of the fetch repeats them: the span of the fetch is started
// before the address is parsed.
func TestEngine_FetchStack_CredentialsNotLogged(t *testing.T) {
	t.Parallel()

	var log bytes.Buffer

	e, err := rigger.New(rigger.WithLogger(slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if err != nil {
		t.Fatal(err)
	}

	_, diags := e.FetchStack(t.Context(), "https://bob:s3cret@git.example.org/acme/stacks//stacks/site", "")
	if !diags.HasErrors() {
		t.Fatal("an address with credentials was taken")
	}

	if got := diags.Error(); strings.Contains(got, "s3cret") {
		t.Errorf("the diagnostic repeats the password: %s", got)
	}

	if got := log.String(); strings.Contains(got, "s3cret") || !strings.Contains(got, "git.example.org") {
		t.Errorf("the log repeats the password, or does not name the host: %s", got)
	}
}
