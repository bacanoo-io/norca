// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// DisruptionBudget limits the voluntary disruptions (node drain, node pool
// scale-down, eviction) of a task group. Exactly one of MinHealthy and
// MaxUnavailable may be set, either as a count ("4") or as a percentage of
// the group count ("25%").
type DisruptionBudget struct {
	MinHealthy     *string `hcl:"min_healthy,optional"`
	MaxUnavailable *string `hcl:"max_unavailable,optional"`
}

// UnmarshalJSON accepts the values both as strings and as JSON numbers.
func (d *DisruptionBudget) UnmarshalJSON(data []byte) error {
	var raw struct {
		MinHealthy     json.RawMessage
		MaxUnavailable json.RawMessage
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var err error
	if d.MinHealthy, err = intOrString(raw.MinHealthy); err != nil {
		return fmt.Errorf("MinHealthy: %w", err)
	}
	if d.MaxUnavailable, err = intOrString(raw.MaxUnavailable); err != nil {
		return fmt.Errorf("MaxUnavailable: %w", err)
	}
	return nil
}

func intOrString(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return &s, nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, errors.New("must be a count or a percentage string")
	}
	s = n.String()
	return &s, nil
}

func (d *DisruptionBudget) Copy() *DisruptionBudget {
	if d == nil {
		return nil
	}
	nd := new(DisruptionBudget)
	if d.MinHealthy != nil {
		nd.MinHealthy = pointerOf(*d.MinHealthy)
	}
	if d.MaxUnavailable != nil {
		nd.MaxUnavailable = pointerOf(*d.MaxUnavailable)
	}
	return nd
}

// DisruptionStatus is the current state of the disruption budget of a task
// group.
type DisruptionStatus struct {
	TaskGroup          string
	MinHealthy         string
	MaxUnavailable     string
	DesiredCount       int
	RequiredHealthy    int
	Healthy            int
	DeploymentReserved int
	DisruptionsAllowed int
}

// Disruption returns the disruption budget status of each task group of the
// job that has a disruption block, keyed by task group name.
func (j *Jobs) Disruption(jobID string, q *QueryOptions) (map[string]*DisruptionStatus, *QueryMeta, error) {
	var resp map[string]*DisruptionStatus
	qm, err := j.client.query("/v1/job/"+url.PathEscape(jobID)+"/disruption", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// AllocEvictResponse is the response to an eviction.
type AllocEvictResponse struct {
	// EvalID is the ID of the follow-up evaluation for the rescheduled
	// allocation. It is empty if the allocation was already being migrated.
	EvalID string

	WriteMeta
}

// Evict stops and reschedules an allocation like a migration if the
// disruption budget of its task group allows it. If the budget does not
// allow it, the returned error satisfies IsDisruptionBudgetExceeded and the
// caller should retry later.
func (a *Allocations) Evict(allocID string, noShutdownDelay bool, w *WriteOptions) (*AllocEvictResponse, error) {
	endpoint := "/v1/allocation/" + url.PathEscape(allocID) + "/evict"
	if noShutdownDelay {
		endpoint += "?no_shutdown_delay=true"
	}

	var resp AllocEvictResponse
	wm, err := a.client.post(endpoint, nil, &resp, w)
	if err != nil {
		return nil, err
	}
	resp.WriteMeta = *wm
	return &resp, nil
}

// IsDisruptionBudgetExceeded returns whether the error is the response of
// the eviction API to an eviction the disruption budget does not allow
// (HTTP 429).
func IsDisruptionBudgetExceeded(err error) bool {
	var uErr UnexpectedResponseError
	return errors.As(err, &uErr) && uErr.StatusCode() == http.StatusTooManyRequests
}
