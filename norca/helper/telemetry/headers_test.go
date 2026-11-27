// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/shoenig/test/must"
)

func writeHeadersFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "otlp-headers")
	must.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestReadHeadersFile(t *testing.T) {
	ci.Parallel(t)

	path := writeHeadersFile(t, strings.Join([]string{
		"# credentials of the backend",
		"",
		"  Authorization = Basic dXNlcjpwYXNz  ",
		"x-api-key=k=with=equals",
		"\tx-tenant =\tteam-a",
	}, "\n"))
	headers, err := readHeadersFile(path)
	must.NoError(t, err)
	must.Eq(t, map[string]string{
		"authorization": "Basic dXNlcjpwYXNz",
		"x-api-key":     "k=with=equals",
		"x-tenant":      "team-a",
	}, headers)
}

func TestReadHeadersFile_Errors(t *testing.T) {
	ci.Parallel(t)

	const secret = "s3cr3t"
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "no equals", content: "authorization " + secret, wantErr: "line 1: not key=value"},
		{name: "reserved", content: "\ngrpc-status=" + secret, wantErr: "line 2"},
		{name: "bad value", content: "a=" + secret + "\x01", wantErr: "value of header"},
		{name: "twice", content: "a=1\nA=" + secret, wantErr: "given twice"},
		{name: "empty name", content: "=" + secret, wantErr: "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readHeadersFile(writeHeadersFile(t, tt.content))
			must.ErrorContains(t, err, tt.wantErr)
			must.StrNotContains(t, err.Error(), secret)
		})
	}

	t.Run("missing", func(t *testing.T) {
		_, err := readHeadersFile(filepath.Join(t.TempDir(), "nope"))
		must.ErrorContains(t, err, "headers_file")
	})
	t.Run("too large", func(t *testing.T) {
		_, err := readHeadersFile(writeHeadersFile(t, strings.Repeat("#", maxHeadersFileSize+1)))
		must.ErrorContains(t, err, "larger than")
	})
}

func TestHeaderCredentials(t *testing.T) {
	ci.Parallel(t)

	path := writeHeadersFile(t, "authorization=Bearer one\nx-file=1")
	h, err := newHeaderCredentials(Target{
		Headers:     map[string]string{"authorization": "inline", "x-inline": "2"},
		HeadersFile: path,
	})
	must.NoError(t, err)
	must.True(t, h.RequireTransportSecurity())

	md, err := h.GetRequestMetadata(t.Context())
	must.NoError(t, err)
	must.Eq(t, map[string]string{"authorization": "Bearer one", "x-file": "1", "x-inline": "2"}, md,
		must.Sprint("the file wins over the inline headers"))

	// the caller may change what it got
	md["authorization"] = "changed"

	// a rotated credential is taken on reload
	must.NoError(t, os.WriteFile(path, []byte("authorization=Bearer two"), 0o600))
	must.NoError(t, h.reload())
	md, err = h.GetRequestMetadata(t.Context())
	must.NoError(t, err)
	must.Eq(t, map[string]string{"authorization": "Bearer two", "x-inline": "2"}, md)

	// a broken file keeps the headers in use
	must.NoError(t, os.WriteFile(path, []byte("broken"), 0o600))
	must.Error(t, h.reload())
	md, err = h.GetRequestMetadata(t.Context())
	must.NoError(t, err)
	must.Eq(t, "Bearer two", md["authorization"])
}

func TestHeaderCredentials_Plaintext(t *testing.T) {
	ci.Parallel(t)

	h, err := newHeaderCredentials(Target{Plaintext: true})
	must.NoError(t, err)
	must.False(t, h.RequireTransportSecurity())
	md, err := h.GetRequestMetadata(t.Context())
	must.NoError(t, err)
	must.MapEmpty(t, md)

	_, err = newHeaderCredentials(Target{HeadersFile: filepath.Join(t.TempDir(), "missing")})
	must.ErrorContains(t, err, "headers_file")
}
