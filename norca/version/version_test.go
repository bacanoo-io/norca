// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package version

import (
	"testing"
	"time"
)

func TestFullVersionNumber(t *testing.T) {
	built := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		info VersionInfo
		rev  bool
		exp  string
	}{
		{
			name: "release",
			info: VersionInfo{Version: "1.6.5"},
			exp:  "Norca v" + NorcaVersion + " (Nomad API 1.6.5 compatible)",
		},
		{
			name: "prerelease and metadata",
			info: VersionInfo{Version: "1.6.5", VersionPrerelease: "dev", VersionMetadata: "ent"},
			exp:  "Norca v" + NorcaVersion + " (Nomad API 1.6.5-dev+ent compatible)",
		},
		{
			name: "build date and revision",
			info: VersionInfo{Version: "1.6.5", BuildDate: built, Revision: "abc123"},
			rev:  true,
			exp: "Norca v" + NorcaVersion + " (Nomad API 1.6.5 compatible)\n" +
				"BuildDate 2026-10-24T12:00:00Z\nRevision abc123",
		},
		{
			name: "revision omitted",
			info: VersionInfo{Version: "1.6.5", Revision: "abc123"},
			exp:  "Norca v" + NorcaVersion + " (Nomad API 1.6.5 compatible)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.FullVersionNumber(tc.rev); got != tc.exp {
				t.Fatalf("expected %q, got %q", tc.exp, got)
			}
		})
	}
}
