// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
)

// A release of a stack changes hands only with Adopt: to the stack of the
// same name in another namespace, to another stack, to no stack, and by a
// rollback. The release API enforces it, whatever planned the request.
func TestRelease_ForeignOwner(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	apply := func(ns, stack string, adopt, dryRun bool) error {
		req := ownedApply(stack, "1.0.0", structs.ReleaseManagerRigger, "sha256:"+ns+stack)
		if req.Owner != nil {
			req.Owner.Namespace = ns
		}

		req.Adopt, req.DryRun = adopt, dryRun

		_, err := c.apply("default", "web", req)

		return err
	}

	must.NoError(t, apply("default", "shop", false, false))
	must.NoError(t, apply("default", "shop", false, false), must.Sprint("its own stack"))
	must.Eq(t, 409, errCode(t, apply("other", "shop", false, false)), must.Sprint("the name in another namespace"))
	must.Eq(t, 409, errCode(t, apply("default", "mall", false, true)), must.Sprint("a dry run"))
	must.Eq(t, 409, errCode(t, apply("default", "mall", false, false)), must.Sprint("another stack"))
	must.Eq(t, 409, errCode(t, apply("", "", false, false)), must.Sprint("no stack"))
	must.Eq(t, "shop", c.get("default", "web").Spec.Owner.Name)

	must.NoError(t, apply("default", "mall", true, false))
	must.Eq(t, "mall", c.get("default", "web").Spec.Owner.Name)

	_, err := c.rollbackAdopt("default", "web", 1, false)
	must.Eq(t, 409, errCode(t, err), must.Sprint("a rollback to another owner"))

	_, err = c.rollbackAdopt("default", "web", 1, true)
	must.NoError(t, err)
	must.Eq(t, "shop", c.get("default", "web").Spec.Owner.Name)

	// The list tells the namespaces apart.
	must.Len(t, 1, c.list("default", "stack/default/shop"))
	must.Len(t, 0, c.list("default", "stack/other/shop"))
}
