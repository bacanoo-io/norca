// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

func externalServiceResource(spec *structs.ExternalServiceSpec) *structs.ReleaseResource {
	return &structs.ReleaseResource{
		Type:            structs.ReleaseResourceExternalService,
		ExternalService: &structs.ReleaseExternalService{Service: spec},
	}
}

// TestRelease_ExternalServices installs a pack with external services: the
// release registers them, plans changes, detects drift, rolls back and
// removes them.
func TestRelease_ExternalServices(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()
	store := c.srv.State()

	node := mock.Node()
	node.Datacenter = "dc1"
	must.NoError(t, store.UpsertNode(structs.MsgTypeTestSetup, 1000, node))

	pack := func(port int) []*structs.ReleaseResource {
		return []*structs.ReleaseResource{
			{Type: structs.ReleaseResourceJob, Job: testReleaseJob("ext-web")},
			externalServiceResource(&structs.ExternalServiceSpec{
				Name: "orders-db", Address: "10.20.0.5", Port: port, Tags: []string{"postgres"},
				Checks: []*structs.ExternalServiceCheck{{
					Type: structs.ExternalCheckTypeTCP, TCP: "10.20.0.5:5432", Interval: 10 * time.Second,
				}},
				CheckedBy: &structs.ExternalCheckedBy{NodePool: "default"},
			}),
			externalServiceResource(&structs.ExternalServiceSpec{
				Name: "billing-db", Kind: structs.ExternalServiceKindAlias, Target: "billing.example.com",
			}),
		}
	}

	resp, err := c.apply("default", "ext", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ext", Version: "1.0.0"}, Resources: pack(5432),
	})
	must.NoError(t, err)
	must.Eq(t, structs.ReleasePhaseDeployed, resp.Release.Status.Phase)
	must.Len(t, 3, resp.Release.Status.Resources)

	db, err := store.GetServiceRegistrationByID(nil, "default", "orders-db")
	must.NoError(t, err)
	must.NotNil(t, db)
	must.Eq(t, structs.ExternalServiceSourceApply, db.External.Source)
	must.Eq(t, 5432, db.Port)
	must.Eq(t, node.ID, db.NodeID, must.Sprint("the servers choose the node that runs the checks"))
	must.Eq(t, "dc1", db.Datacenter)
	must.Eq(t, time.Duration(0), db.External.DeregisterCriticalAfter)
	alias, _ := store.GetServiceRegistrationByID(nil, "default", "billing-db")
	must.NotNil(t, alias)
	must.Eq(t, "billing.example.com", alias.Address)

	// the same pack again: nothing to do, also for the datacenter that the
	// node gave to the service
	plan, err := c.apply("default", "ext", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ext", Version: "1.0.0"}, Resources: pack(5432), DryRun: true,
	})
	must.NoError(t, err)
	actions := map[string]string{}
	for _, ch := range plan.Changes {
		actions[ch.Type+":"+ch.Name] = ch.Action
	}
	must.Eq(t, structs.ReleaseChangeUnchanged, actions["external-service:orders-db"])
	must.Eq(t, structs.ReleaseChangeUnchanged, actions["external-service:billing-db"])

	// an upgrade changes one service
	_, err = c.apply("default", "ext", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ext", Version: "1.1.0"}, Resources: pack(6432),
	})
	must.NoError(t, err)
	db, _ = store.GetServiceRegistrationByID(nil, "default", "orders-db")
	must.Eq(t, 6432, db.Port)

	// a change made by hand is drift
	must.False(t, c.drift("default", "ext").Drifted)
	edited := db.Copy()
	edited.Tags = []string{"edited"}
	edited.External.Source = structs.ExternalServiceSourceAPI
	var upsert structs.ExternalServiceUpsertResponse
	must.NoError(t, msgpackrpc.CallWithCodec(c.codec, structs.ServiceRegistrationUpsertExternalRPCMethod,
		&structs.ExternalServiceUpsertRequest{
			Service:      edited,
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default", AuthToken: c.token},
		}, &upsert))
	d := c.drift("default", "ext")
	must.True(t, d.Drifted)
	must.Len(t, 1, d.Resources)
	must.Eq(t, "orders-db", d.Resources[0].Name)

	// rolling back restores revision 1
	_, err = c.rollback("default", "ext", 1)
	must.NoError(t, err)
	db, _ = store.GetServiceRegistrationByID(nil, "default", "orders-db")
	must.Eq(t, 5432, db.Port)
	must.Eq(t, []string{"postgres"}, db.Tags)
	must.Eq(t, structs.ExternalServiceSourceApply, db.External.Source)
	must.False(t, c.drift("default", "ext").Drifted)

	// a service dropped from the pack is deregistered by the upgrade
	_, err = c.apply("default", "ext", &structs.ReleaseApplyRequest{
		Pack: &structs.ReleasePack{Name: "ext", Version: "1.2.0"}, Resources: pack(5432)[:2],
	})
	must.NoError(t, err)
	alias, _ = store.GetServiceRegistrationByID(nil, "default", "billing-db")
	must.Nil(t, alias)

	// uninstall removes the rest, services after the jobs
	del, err := c.uninstall("default", "ext", "")
	must.NoError(t, err)
	var order []string
	for _, ch := range del.Changes {
		must.Eq(t, "", ch.Error)
		order = append(order, ch.Type)
	}
	must.Eq(t, []string{"job", "external-service"}, order)
	db, _ = store.GetServiceRegistrationByID(nil, "default", "orders-db")
	must.Nil(t, db)
}

func TestRelease_ExternalServiceValidation(t *testing.T) {
	ci.Parallel(t)
	c, cleanup := newReleaseTestClient(t)
	defer cleanup()

	apply := func(spec *structs.ExternalServiceSpec) error {
		_, err := c.apply("default", "bad", &structs.ReleaseApplyRequest{
			Pack:      &structs.ReleasePack{Name: "bad"},
			Resources: []*structs.ReleaseResource{externalServiceResource(spec)},
		})
		return err
	}

	// node checks need checked_by
	must.ErrorContains(t, apply(&structs.ExternalServiceSpec{
		Name: "x", Address: "10.0.0.1",
		Checks: []*structs.ExternalServiceCheck{{Type: "tcp", TCP: "10.0.0.1:80", Interval: time.Second}},
	}), "need checked_by")

	// an alias needs a DNS name
	must.ErrorContains(t, apply(&structs.ExternalServiceSpec{
		Name: "y", Kind: structs.ExternalServiceKindAlias, Target: "10.0.0.1",
	}), "must be a DNS name")

	// the resource has a name
	must.ErrorContains(t, apply(&structs.ExternalServiceSpec{Address: "10.0.0.1"}), "no payload or no name")
}
