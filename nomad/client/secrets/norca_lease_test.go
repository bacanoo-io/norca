// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

// leaseRPC serves the lease methods from memory.
type leaseRPC struct {
	lease   *structs.SecretLease
	creates []*structs.SecretLeaseCreateRequest
	revoked []string
	err     error
}

func (f *leaseRPC) RPC(method string, args, reply any) error {
	if f.err != nil {
		return f.err
	}
	switch method {
	case structs.SecretLeaseCreateRPCMethod:
		f.creates = append(f.creates, args.(*structs.SecretLeaseCreateRequest))
		r := reply.(*structs.SecretLeaseCreateResponse)
		r.Lease = f.lease.Copy()
		r.Items = structs.SecretItems{"creds": "file"}
		r.File, r.FileItem = "nats.creds", "creds"
	case structs.SecretLeaseRenewRPCMethod:
		r := reply.(*structs.SecretLeaseWriteResponse)
		f.lease.ExpireTime = f.lease.MaxExpireTime
		r.Lease = f.lease.Copy()
	case structs.SecretLeaseRevokeRPCMethod:
		f.revoked = append(f.revoked, args.(*structs.SecretLeaseRevokeRequest).LeaseID)
	default:
		return errors.New("unknown method")
	}
	return nil
}

func TestNorcaProvider_Leases(t *testing.T) {
	ci.Parallel(t)
	now := time.Now()
	rpc := &leaseRPC{lease: &structs.SecretLease{ID: "l1", ExpireTime: now.Add(time.Minute).UnixNano(),
		MaxExpireTime: now.Add(time.Hour).UnixNano()}}
	p := NewNorcaProvider(rpc, "global")
	must.True(t, p.Capabilities().Leases)
	id := &Identity{Token: "wi-token", Namespace: "default"}
	ctx := context.Background()

	s, err := p.Fetch(ctx, &Request{Engine: "nats", Role: "worker", Name: "nats", Identity: id})
	must.NoError(t, err)
	must.Eq(t, "file", s.Items["creds"])
	must.Eq(t, "nats.creds", s.DefaultFile)
	must.Eq(t, "creds", s.DefaultFileItem)
	must.Eq(t, "l1", s.Lease.ID)
	must.True(t, s.Lease.Renewable)
	must.Eq(t, now.Add(time.Hour).UnixNano(), s.Lease.MaxExpireTime.UnixNano())
	must.Eq(t, "wi-token", rpc.creates[0].AuthToken)
	must.Eq(t, "nats", rpc.creates[0].SecretName)

	renewed, err := p.Renew(ctx, s.Lease, id)
	must.NoError(t, err)
	must.False(t, renewed.Renewable)
	must.Eq(t, renewed.MaxExpireTime, renewed.ExpireTime)
	must.NoError(t, p.Revoke(ctx, renewed, id))
	must.Eq(t, []string{"l1"}, rpc.revoked)

	_, err = p.Fetch(ctx, &Request{Engine: "n a", Role: "worker", Identity: id})
	must.Eq(t, KindInvalid, KindOf(err))
	_, err = p.Watch(ctx, &Request{Engine: "nats", Role: "worker", Identity: id}, "")
	must.Eq(t, KindInvalid, KindOf(err))

	rpc.err = errors.New("RPC Error:: 404,secret lease is revoked")
	_, err = p.Renew(ctx, s.Lease, id)
	must.Eq(t, KindNotFound, KindOf(err))
}
