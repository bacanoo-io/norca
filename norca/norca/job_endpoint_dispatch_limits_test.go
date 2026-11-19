// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"strings"
	"testing"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
)

// The IdPrefixTemplate of a dispatch becomes part of the ID of the
// dispatched job, which is registered without Job.Validate, and the meta is
// stored with it (review finding SEC-31; inherited). A token with
// dispatch-job could make jobs with a space, a null character or a line
// break in their ID, of any length, and store any amount of meta, while the
// payload is limited to DispatchPayloadSizeLimit. The prefix now follows the
// rules of a job ID and is at most DispatchIDPrefixTemplateLimit bytes; the
// meta is at most DispatchMetaSizeLimit bytes.
func TestJobEndpoint_Dispatch_Limits(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := TestServer(t, func(c *Config) { c.NumSchedulers = 0 })
	defer cleanup()
	codec := rpcClient(t, s)
	testutil.WaitForLeader(t, s.RPC)

	job := mock.BatchJob()
	job.ParameterizedJob = &structs.ParameterizedJobConfig{MetaOptional: []string{"note", "other"}}
	var regResp structs.JobRegisterResponse
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Job.Register", &structs.JobRegisterRequest{
		Job:          job,
		WriteRequest: structs.WriteRequest{Region: "global", Namespace: job.Namespace},
	}, &regResp))

	dispatch := func(prefix string, meta map[string]string) (string, error) {
		var resp structs.JobDispatchResponse
		err := msgpackrpc.CallWithCodec(codec, "Job.Dispatch", &structs.JobDispatchRequest{
			JobID:            job.ID,
			IdPrefixTemplate: prefix,
			Meta:             meta,
			WriteRequest:     structs.WriteRequest{Region: "global", Namespace: job.Namespace},
		}, &resp)
		return resp.DispatchedJobID, err
	}

	cases := []struct {
		name   string
		prefix string
		meta   map[string]string
		err    string
	}{
		{name: "plain prefix", prefix: "nightly-run.1"},
		{name: "prefix at the limit", prefix: strings.Repeat("p", DispatchIDPrefixTemplateLimit)},
		{name: "prefix too long", prefix: strings.Repeat("p", DispatchIDPrefixTemplateLimit+1),
			err: "ID prefix template exceeds maximum length; 129 > 128"},
		{name: "space", prefix: "a b", err: "contains a space or a control character"},
		{name: "null character", prefix: "a\x00b", err: "contains a space or a control character"},
		{name: "line break", prefix: "a\nfake log line", err: "contains a space or a control character"},
		{name: "tab", prefix: "a\tb", err: "contains a space or a control character"},
		{name: "escape", prefix: "a\x1b[31mb", err: "contains a space or a control character"},
		{name: "invalid UTF-8", prefix: "a\xffb", err: "not valid UTF-8"},
		{name: "unicode letters", prefix: "éjjeli"},
		{name: "meta at the limit",
			meta: map[string]string{"note": strings.Repeat("m", DispatchMetaSizeLimit-len("note"))}},
		{name: "meta too big",
			meta: map[string]string{
				"note":  strings.Repeat("m", DispatchMetaSizeLimit/2),
				"other": strings.Repeat("m", DispatchMetaSizeLimit/2),
			},
			err: "Meta exceeds maximum size; 16393 > 16384"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := s.State().JobsByIDPrefix(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			countBefore := 0
			for raw := before.Next(); raw != nil; raw = before.Next() {
				countBefore++
			}

			id, err := dispatch(tc.prefix, tc.meta)
			if tc.err == "" {
				must.NoError(t, err)
				must.StrContains(t, id, tc.prefix)
				return
			}
			must.ErrorContains(t, err, tc.err)

			// nothing is registered
			after, err := s.State().JobsByIDPrefix(nil, job.Namespace, job.ID)
			must.NoError(t, err)
			countAfter := 0
			for raw := after.Next(); raw != nil; raw = after.Next() {
				countAfter++
			}
			must.Eq(t, countBefore, countAfter)
		})
	}
}
