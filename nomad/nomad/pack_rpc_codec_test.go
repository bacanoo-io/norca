// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/hashicorp/go-msgpack/codec"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// TestPackRPC_MsgpackRoundTrip checks that the pack RPC types keep their own
// fields on the wire. With -tags codegen_generated (release builds, CI) the
// embedded structs.WriteRequest and its kin have generated codecs, which the
// types would otherwise inherit, dropping everything else (a follower then
// forwarded Packs.Upsert without its Registry).
func TestPackRPC_MsgpackRoundTrip(t *testing.T) {
	ci.Parallel(t)

	write := structs.WriteRequest{Region: "mdc", Namespace: "default", AuthToken: "secret"}
	query := structs.QueryOptions{Region: "mdc", Namespace: "default", AuthToken: "secret", MinQueryIndex: 7}
	reg := &PackRegistry{Name: "lab", Spec: &PackRegistrySpec{Type: "dir", Source: "/opt/registry", DefaultRef: "latest"}}

	cases := []any{
		&PackRegistryListRequest{QueryOptions: query},
		&PackRegistryListResponse{Registries: []*PackRegistry{reg}, QueryMeta: structs.QueryMeta{Index: 3, KnownLeader: true}},
		&PackRegistryUpsertRequest{Registry: reg, WriteRequest: write},
		&PackRegistryNameRequest{Name: "lab", WriteRequest: write},
		&PackRegistryResponse{Registry: reg, WriteMeta: structs.WriteMeta{Index: 4}},
		&PackDetailRequest{Registry: "lab", Pack: "nats", Version: "0.4.0", QueryOptions: query},
		&PackDetailRPCResponse{Detail: &PackDetailResponse{}, QueryMeta: structs.QueryMeta{Index: 5}},
		&PackDeployRPCRequest{Name: "nats", Deploy: &PackDeployRequest{Registry: "lab", Pack: "nats", Values: map[string]any{"count": "3"}}, DryRun: true, WriteRequest: write},
		&PackUploadRequest{Registry: "lab", FileName: "nats.tar.gz", Data: []byte{1, 2, 3}, WriteRequest: write},
		&PackUploadDeleteRequest{Registry: "lab", Digest: "sha256:00", WriteRequest: write},
		&PackUploadResponse{Registry: reg, Digest: "sha256:00", WriteMeta: structs.WriteMeta{Index: 6}},
	}
	for _, in := range cases {
		typ := reflect.TypeOf(in).Elem()
		t.Run(typ.Name(), func(t *testing.T) {
			var buf bytes.Buffer
			must.NoError(t, codec.NewEncoder(&buf, structs.MsgpackHandle).Encode(in))
			out := reflect.New(typ).Interface()
			must.NoError(t, codec.NewDecoder(&buf, structs.MsgpackHandle).Decode(out))
			must.Eq(t, in, out)
		})
	}
}
