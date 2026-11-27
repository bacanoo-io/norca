// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
)

// A scheduler worker configuration with the same number of workers but
// another number of enabled schedulers is a change: it used to index past
// the shorter list and panic.
func TestShouldReloadSchedulers_EnabledLength(t *testing.T) {
	ci.Parallel(t)

	s := &Server{config: &Config{
		NumSchedulers:     1,
		EnabledSchedulers: []string{structs.JobTypeCore, structs.JobTypeService},
	}}
	cases := []struct {
		name    string
		enabled []string
		reload  bool
	}{
		{name: "same", enabled: []string{structs.JobTypeService, structs.JobTypeCore}, reload: false},
		{name: "more", enabled: []string{structs.JobTypeCore, structs.JobTypeService, structs.JobTypeBatch}, reload: true},
		{name: "fewer", enabled: []string{structs.JobTypeCore}, reload: true},
		{name: "other", enabled: []string{structs.JobTypeCore, structs.JobTypeBatch}, reload: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reload, args := shouldReloadSchedulers(s, &SchedulerWorkerPoolArgs{NumSchedulers: 1, EnabledSchedulers: tc.enabled})
			must.Eq(t, tc.reload, reload)
			must.Eq(t, tc.reload, args != nil)
		})
	}
}
