// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norcaapi_test

import (
	"os"
	"testing"

	rig "github.com/bacanoo-io/norca-rig"
	"github.com/bacanoo-io/norca-rig/cli/norcaapi"
)

// TestEngine_ValidateModule_Examples renders the value files of the
// examples with the job decoder of Norca (design 7.4).
func TestEngine_ValidateModule_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{
		"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			eng, err := rig.New(rig.WithJobDecoder(norcaapi.JobDecoder{}))
			if err != nil {
				t.Fatal(err)
			}

			m, diags := eng.LoadModuleAt(t.Context(), os.DirFS("../../examples"), dir)
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if diags := eng.ValidateModule(t.Context(), m); diags.HasErrors() {
				t.Error(diags)
			}
		})
	}
}
