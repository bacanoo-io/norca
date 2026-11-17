// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package jobspec2

import "testing"

// The same file through today's exported entry point, without the spike's.
func TestRigSpike_ParseWithConfigAsItIs(t *testing.T) {
	_, err := ParseWithConfig(&ParseConfig{Path: "web.norca.hcl", Body: []byte(rigJobFile), Strict: true})
	t.Logf("ParseWithConfig: %v", err)

	// Without image_ref and the var/local blocks of the module.
	src := `job "web" {
  meta {
    release = rig.release
  }
  group "web" {
    task "web" {
      driver = "docker"
      config { image = "x" }
    }
  }
}`
	job, err := ParseWithConfig(&ParseConfig{Path: "web.norca.hcl", Body: []byte(src), Strict: true})
	if err != nil {
		t.Fatalf("ParseWithConfig: %v", err)
	}
	t.Logf("rig.release became meta %q", job.Meta["release"])
}
