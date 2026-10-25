package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteProm(t *testing.T) {
	m := New()
	m.RegisterGauge("ubixvault_build_info", "Build information.", func() float64 { return 1 },
		[2]string{"version", "0.2.0-beta.2"})
	m.RegisterGauge("ubixvault_sealed", "Sealed.", func() float64 { return 0 })
	m.ObserveRequest(200)
	m.ObserveRequest(200)
	m.ObserveRequest(404)

	var b bytes.Buffer
	m.WriteProm(&b)
	out := b.String()

	for _, want := range []string{
		"# TYPE ubixvault_build_info gauge",
		`ubixvault_build_info{version="0.2.0-beta.2"} 1`,
		"ubixvault_sealed 0",
		"# TYPE ubixvault_http_requests_total counter",
		`ubixvault_http_requests_total{code="200"} 2`,
		`ubixvault_http_requests_total{code="404"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestEscapeLabelValue(t *testing.T) {
	m := New()
	m.RegisterGauge("g", "h", func() float64 { return 1 }, [2]string{"v", `a"b\c` + "\n"})
	var b bytes.Buffer
	m.WriteProm(&b)
	if got := b.String(); !strings.Contains(got, `v="a\"b\\c\n"`) {
		t.Fatalf("label not escaped: %q", got)
	}
}
