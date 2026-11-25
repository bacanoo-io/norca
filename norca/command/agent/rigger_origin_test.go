// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"

	"github.com/bacanoo-io/norca/norca/api"
	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/testutil"
	rigger "github.com/bacanoo-io/norca/rigger"
)

// A client that fetched a stack from a repository sends where it came from
// with the files: the operation records the repository, the ref, the commit
// and the signer, and each release of the stack's own modules the commit.
// The same stack at a later commit changes no release, and they keep the
// commit that last changed them. The origin is of a stack sent as files,
// and a repository with credentials in its address is refused.
func TestHTTP_RiggerStackOrigin(t *testing.T) {
	ci.Parallel(t)

	eng, err := rigger.New()
	must.NoError(t, err)

	st, diags := eng.LoadStack(t.Context(), stackOpFiles, "s")
	must.False(t, diags.HasErrors(), must.Sprint(diags))

	files, err := st.Archive()
	must.NoError(t, err)

	withBus := func(c *Config) { c.Server.Bus = &BusConfig{Enabled: pointer.Of(true)} }

	httpACLTest(t, withBus, func(s *TestAgent) {
		testutil.WaitForLeader(t, s.RPC)

		client := s.Client()
		client.SetSecretID(s.RootToken.SecretID)

		first, second := strings.Repeat("1", 40), strings.Repeat("2", 40)
		origin := &api.RiggerStackOrigin{
			Repository: "https://git.example.org/acme/platform-env.git", Ref: "main", Commit: first, Signer: "ops@example.org",
		}
		req := &api.RiggerStackRequest{Files: files, Origin: origin}

		// The kinds of Rigger are registered by the leader after its
		// election.
		must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
			_, _, err := client.Rigger().StackPlan(req, nil)

			return err
		}), wait.Timeout(30*time.Second), wait.Gap(100*time.Millisecond)))

		started, _, err := client.Rigger().StackApply(req, nil)
		must.NoError(t, err)
		must.NotNil(t, started.Spec.Origin, must.Sprint("the operation records no origin"))
		must.Eq(t, *origin, *started.Spec.Origin)

		op := waitOperation(t, client, started.Name)
		must.Eq(t, "Succeeded", op.Status.Phase, must.Sprint(op.Status.Error))
		must.Eq(t, *origin, *op.Spec.Origin)

		commitOf := func(release string) string {
			rel, _, err := client.Releases().Info(release, nil)
			must.NoError(t, err)
			must.NotNil(t, rel.Spec.Pack)

			return rel.Spec.Pack.Commit
		}

		for _, release := range []string{"db", "app"} {
			must.Eq(t, first, commitOf(release), must.Sprintf("the commit of the release %s", release))
		}

		// The same files at a later commit: nothing to do, and the releases
		// say when they last changed.
		later := &api.RiggerStackRequest{Files: files, Origin: &api.RiggerStackOrigin{
			Repository: origin.Repository, Ref: "main", Commit: second,
		}}

		plan, _, err := client.Rigger().StackPlan(later, nil)
		must.NoError(t, err)
		must.Eq(t, "db:unchanged app:unchanged", stackActions(plan.Plan.Releases))

		again, _, err := client.Rigger().StackApply(later, nil)
		must.NoError(t, err)
		must.Eq(t, second, again.Spec.Origin.Commit)
		must.Eq(t, "Succeeded", waitOperation(t, client, again.Name).Status.Phase)
		must.Eq(t, first, commitOf("db"))

		// What a request may not say.
		for _, tc := range []struct {
			name string
			req  *api.RiggerStackRequest
			want string
		}{
			{
				name: "an origin of a published stack",
				req:  &api.RiggerStackRequest{Source: "acme/platform", Origin: origin},
				want: "the origin is of a stack sent as files",
			},
			{
				name: "credentials in the repository",
				req: &api.RiggerStackRequest{Files: files, Origin: &api.RiggerStackOrigin{
					Repository: "https://ci:s3cret@git.example.org/acme/platform-env.git", Commit: first,
				}},
				want: "credentials",
			},
			{
				name: "a commit that is none",
				req:  &api.RiggerStackRequest{Files: files, Origin: &api.RiggerStackOrigin{Commit: "main; rm -rf /"}},
				want: "commit",
			},
			{
				name: "a line break in the signer",
				req:  &api.RiggerStackRequest{Files: files, Origin: &api.RiggerStackOrigin{Commit: first, Signer: "ops\nroot"}},
				want: "signer",
			},
		} {
			_, _, err := client.Rigger().StackApply(tc.req, nil)
			must.ErrorContains(t, err, tc.want, must.Sprint(tc.name))
			must.ErrorContains(t, err, "400", must.Sprint(tc.name))
		}
	})
}
