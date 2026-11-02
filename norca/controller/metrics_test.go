// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shoenig/test/must"
)

func TestTextMetrics(t *testing.T) {
	m := NewTextMetrics()
	m.ReconcileDone("lb", 2*time.Second, ResultSuccess)
	m.ReconcileDone("lb", time.Second, ResultError)
	m.QueueDepth("lb", 3)
	m.Resync("lb", "objects", "gone")
	m.LeaderChanged("lb", true)

	must.Eq(t, 1, m.Value("norca_controller_reconcile_total", "controller", "lb", "result", ResultError))
	must.Eq(t, 3, m.Value("norca_controller_reconcile_seconds_sum", "controller", "lb"))
	must.Eq(t, 3, m.Value("norca_controller_queue_depth", "controller", "lb"))

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	must.True(t, strings.Contains(body, `norca_controller_resync_total{controller="lb",source="objects",reason="gone"} 1`), must.Sprint(body))
	must.True(t, strings.Contains(body, `norca_controller_leader{controller="lb"} 1`), must.Sprint(body))
}
