// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"testing"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func TestJobs_ApiAllocAffinitiesToStructs(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, ApiAllocAffinitiesToStructs(nil))

	job := &api.Job{
		ID:   pointer.Of("cache"),
		Name: pointer.Of("cache"),
		Type: pointer.Of("service"),
		TaskGroups: []*api.TaskGroup{{
			Name: pointer.Of("redis"),
			Tasks: []*api.Task{{
				Name:   "redis",
				Driver: "docker",
			}},
			Colocate: []*api.AllocAffinity{
				api.NewAllocAffinity(map[string]string{"app": "api"}, "", 30),
			},
			Separate: []*api.AllocAffinity{{
				Selector:  map[string]string{"app": "cache"},
				Namespace: "other",
				Scope:     "${meta.zone}",
				Required:  true,
			}},
		}},
	}
	job.Canonicalize()

	out := ApiJobToStructJob(job)
	tg := out.TaskGroups[0]
	must.Eq(t, []*structs.AllocAffinity{{
		Selector: map[string]string{"app": "api"},
		Scope:    structs.AllocAffinityDefaultScope,
		Weight:   30,
	}}, tg.Colocate)
	must.Eq(t, []*structs.AllocAffinity{{
		Selector:  map[string]string{"app": "cache"},
		Namespace: "other",
		Scope:     "${meta.zone}",
		Required:  true,
	}}, tg.Separate)

	// the conversion does not share the selector map
	job.TaskGroups[0].Colocate[0].Selector["app"] = "changed"
	must.Eq(t, "api", tg.Colocate[0].Selector["app"])
}
