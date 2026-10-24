// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"sort"

	"github.com/hashicorp/nomad/api"
)

// outputDisruptionBudgets prints the current state of the disruption budgets
// of the job's task groups, if any group has one.
func (c *JobStatusCommand) outputDisruptionBudgets(client *api.Client, job *api.Job, q *api.QueryOptions) {
	uses := false
	for _, tg := range job.TaskGroups {
		if tg.Disruption != nil {
			uses = true
			break
		}
	}
	if !uses {
		return
	}

	c.Ui.Output(c.Colorize().Color("\n[bold]Disruption Budgets[reset]"))
	statuses, _, err := client.Jobs().Disruption(*job.ID, q)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error querying disruption budgets: %s", err))
		return
	}
	c.Ui.Output(formatDisruptionStatuses(statuses))
}

func formatDisruptionStatuses(statuses map[string]*api.DisruptionStatus) string {
	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([]string, 0, len(names)+1)
	rows = append(rows, "Task Group|Budget|Healthy|Required|Reserved|Disruptions Allowed")
	for _, name := range names {
		s := statuses[name]
		budget := "min_healthy = " + s.MinHealthy
		if s.MinHealthy == "" {
			budget = "max_unavailable = " + s.MaxUnavailable
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%d|%d|%d|%d",
			name, budget, s.Healthy, s.RequiredHealthy, s.DeploymentReserved, s.DisruptionsAllowed))
	}
	return formatList(rows)
}
