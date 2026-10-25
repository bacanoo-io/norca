// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package externalsvc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// outputLimit is the number of bytes of an HTTP response kept as output.
const outputLimit = 4096

// runnerKey identifies a check of an owned registration.
type runnerKey struct {
	namespace string
	id        string
	check     string
}

// runner runs one http or tcp check at its interval and reports its status
// when it differs from the status last recorded by the servers.
type runner struct {
	agent *Agent
	key   runnerKey
	def   *structs.ExternalServiceCheck

	lock     sync.Mutex
	reported string

	cancel context.CancelFunc
	done   chan struct{}
}

func newRunner(agent *Agent, key runnerKey, def *structs.ExternalServiceCheck, reported string) *runner {
	return &runner{agent: agent, key: key, def: def, reported: reported, done: make(chan struct{})}
}

// setReported records the status the servers hold.
func (r *runner) setReported(status string) {
	r.lock.Lock()
	r.reported = status
	r.lock.Unlock()
}

func (r *runner) start() {
	ctx, cancel := context.WithCancel(r.agent.ctx)
	r.cancel = cancel
	go r.run(ctx)
}

func (r *runner) stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *runner) run(ctx context.Context) {
	defer close(r.done)

	// spread the first runs of the checks of a node
	wait := time.Duration(rand.Int63n(int64(min(r.def.Interval, 2*time.Second)) + 1))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = r.def.Interval

		status, output := Run(ctx, r.def)
		if ctx.Err() != nil {
			return
		}

		r.lock.Lock()
		changed := status != r.reported
		r.lock.Unlock()
		if !changed {
			continue
		}
		if err := r.agent.report(r.key, status, output); err != nil {
			r.agent.logger.Debug("failed to report check status", "service_id", r.key.id,
				"check_id", r.key.check, "status", status, "error", err)
			continue
		}
		r.setReported(status)
	}
}

// Run runs an http or tcp check once and returns its status and output.
func Run(ctx context.Context, def *structs.ExternalServiceCheck) (string, string) {
	timeout := def.Timeout
	if timeout <= 0 {
		timeout = structs.DefaultExternalCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch def.Type {
	case structs.ExternalCheckTypeHTTP:
		return runHTTP(ctx, def)
	case structs.ExternalCheckTypeTCP:
		return runTCP(ctx, def)
	default:
		return structs.ServiceHealthCritical, fmt.Sprintf("unsupported check type %q", def.Type)
	}
}

// runHTTP runs an http check: a 2xx response passes, 429 Too Many Requests
// warns, anything else is critical.
func runHTTP(ctx context.Context, def *structs.ExternalServiceCheck) (string, string) {
	method := def.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if def.Body != "" {
		body = strings.NewReader(def.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, def.HTTP, body)
	if err != nil {
		return structs.ServiceHealthCritical, err.Error()
	}
	for name, values := range def.Header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if host := req.Header.Get("Host"); host != "" {
		req.Host = host
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "Norca Health Check")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: def.TLSSkipVerify,
		ServerName:         def.TLSServerName,
	}
	client := &http.Client{Transport: transport}

	resp, err := client.Do(req)
	if err != nil {
		return structs.ServiceHealthCritical, fmt.Sprintf("HTTP %s %s: %v", method, def.HTTP, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, outputLimit))
	output := fmt.Sprintf("HTTP %s %s: %s Output: %s", method, def.HTTP, resp.Status, out)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return structs.ServiceHealthPassing, output
	case resp.StatusCode == http.StatusTooManyRequests:
		return structs.ServiceHealthWarning, output
	default:
		return structs.ServiceHealthCritical, output
	}
}

// runTCP runs a tcp check: it passes when a connection can be established.
func runTCP(ctx context.Context, def *structs.ExternalServiceCheck) (string, string) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", def.TCP)
	if err != nil {
		return structs.ServiceHealthCritical, fmt.Sprintf("TCP connect %s: %v", def.TCP, err)
	}
	conn.Close()
	return structs.ServiceHealthPassing, fmt.Sprintf("TCP connect %s: Success", def.TCP)
}
