// Package metrics is a tiny, dependency-free Prometheus text-format exporter.
//
// uBixVault deliberately keeps a small, auditable dependency graph, so rather
// than pull in a metrics client library this package renders the handful of
// series the vault exposes directly in the Prometheus text exposition format.
// It supports two shapes: counters incremented inline, and gauges gathered from
// a callback at scrape time (so seal state and uptime are always current).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Metrics collects the vault's operational series. The zero value is not usable;
// call [New].
type Metrics struct {
	mu            sync.Mutex
	requests      map[int]uint64    // HTTP requests by status code
	quotaExceeded map[string]uint64 // rate-limit quota denials by quota name
	gauges        []gauge           // gathered at scrape time, in registration order
}

type gauge struct {
	name   string
	help   string
	labels [][2]string
	fn     func() float64
}

// New returns an empty registry.
func New() *Metrics {
	return &Metrics{requests: make(map[int]uint64), quotaExceeded: make(map[string]uint64)}
}

// RegisterGauge adds a gauge whose value is read from fn each time metrics are
// rendered. Optional label pairs are attached verbatim.
func (m *Metrics) RegisterGauge(name, help string, fn func() float64, labels ...[2]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges = append(m.gauges, gauge{name: name, help: help, labels: labels, fn: fn})
}

// ObserveRequest records one HTTP response with the given status code.
func (m *Metrics) ObserveRequest(code int) {
	m.mu.Lock()
	m.requests[code]++
	m.mu.Unlock()
}

// ObserveQuotaExceeded records one request denied by a rate-limit quota, labeled
// by quota name ("default" for the default/global limit).
func (m *Metrics) ObserveQuotaExceeded(name string) {
	m.mu.Lock()
	m.quotaExceeded[name]++
	m.mu.Unlock()
}

// WriteProm renders all series in Prometheus text exposition format. Write
// errors on w are ignored (metrics must never fail the caller).
func (m *Metrics) WriteProm(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, g := range m.gauges {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", g.name, g.help)
		_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", g.name)
		_, _ = fmt.Fprintf(w, "%s%s %s\n", g.name, formatLabels(g.labels), formatFloat(g.fn()))
	}

	if len(m.requests) > 0 {
		const name = "ubixvault_http_requests_total"
		_, _ = fmt.Fprintf(w, "# HELP %s Total HTTP requests handled, by response status code.\n", name)
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		codes := make([]int, 0, len(m.requests))
		for code := range m.requests {
			codes = append(codes, code)
		}
		sort.Ints(codes)
		for _, code := range codes {
			_, _ = fmt.Fprintf(w, "%s{code=\"%d\"} %d\n", name, code, m.requests[code])
		}
	}

	if len(m.quotaExceeded) > 0 {
		const name = "ubixvault_quota_exceeded_total"
		_, _ = fmt.Fprintf(w, "# HELP %s Total requests denied by a rate-limit quota, by quota name.\n", name)
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		quotas := make([]string, 0, len(m.quotaExceeded))
		for q := range m.quotaExceeded {
			quotas = append(quotas, q)
		}
		sort.Strings(quotas)
		for _, q := range quotas {
			_, _ = fmt.Fprintf(w, "%s{quota=\"%s\"} %d\n", name, escapeLabelValue(q), m.quotaExceeded[q])
		}
	}
}

// formatLabels renders {k="v",...}, or "" when there are no labels.
func formatLabels(labels [][2]string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range labels {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=\"%s\"", l[0], escapeLabelValue(l[1]))
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue escapes the three characters the text format reserves in a
// label value: backslash, double-quote, and newline.
func escapeLabelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// formatFloat renders a float without a trailing ".0" for whole numbers, so
// gauges like 0/1 read cleanly.
func formatFloat(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%g", f), ".0")
}
