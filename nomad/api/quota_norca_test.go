// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/hashicorp/nomad/api/internal/testutil"
	"github.com/shoenig/test/must"
)

func TestEvent_QuotaPayloads(t *testing.T) {
	testutil.Parallel(t)

	raw := []byte(`{
		"Topic": "Quota",
		"Type": "QuotaExhausted",
		"Key": "team-a",
		"Index": 10,
		"Payload": {
			"QuotaUsage": {
				"Name": "team-a",
				"Used": {
					"aGFzaA==": {
						"Region": "global",
						"RegionLimit": {"CPU": 1000, "MemoryMB": 512},
						"Allocations": 3,
						"Hash": "aGFzaA=="
					}
				}
			},
			"Dimensions": ["allocations"]
		}
	}`)

	var event Event
	must.NoError(t, json.Unmarshal(raw, &event))
	must.Eq(t, TopicQuota, event.Topic)

	usage, dims, err := event.QuotaUsage()
	must.NoError(t, err)
	must.Eq(t, []string{"allocations"}, dims)
	must.Eq(t, "team-a", usage.Name)
	used := usage.Used["aGFzaA=="]
	must.NotNil(t, used)
	must.Eq(t, 3, *used.Allocations)
	must.Eq(t, 1000, *used.RegionLimit.CPU)

	raw = []byte(`{
		"Topic": "Quota",
		"Type": "QuotaSpecUpserted",
		"Key": "team-a",
		"Payload": {
			"QuotaSpec": {
				"Name": "team-a",
				"Enforce": "submit",
				"Limits": [{"Region": "global", "Allocations": 5, "Hash": "aGFzaA=="}],
				"TaskMax": {"CPU": 4000}
			}
		}
	}`)
	event = Event{}
	must.NoError(t, json.Unmarshal(raw, &event))
	spec, err := event.QuotaSpec()
	must.NoError(t, err)
	must.Eq(t, "submit", spec.Enforce)
	must.Eq(t, 5, *spec.Limits[0].Allocations)
	must.Eq(t, 4000, *spec.TaskMax.CPU)
}
