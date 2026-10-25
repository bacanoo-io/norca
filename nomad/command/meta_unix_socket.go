// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"
)

// unixSocketPrefix marks an agent address that is a unix domain socket, e.g.
// the task API socket of a task: unix://${NOMAD_SECRETS_DIR}/api.sock.
const unixSocketPrefix = "unix://"

// configureUnixSocket makes the client talk HTTP over the unix socket when the
// address is unix://<path>. Tasks use it to run CLI commands with their
// workload identity (NOMAD_TOKEN) through the task API, for example
// "norca var put" in a bootstrap task. Other addresses are left unchanged.
func configureUnixSocket(conf *api.Config) error {
	socket, ok := strings.CutPrefix(conf.Address, unixSocketPrefix)
	if !ok {
		return nil
	}
	// Not parsed as a URL: on Windows the path is a drive path
	// (unix://C:\...\secrets\api.sock).
	if socket == "" {
		return errors.New("the unix socket address has no path")
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conf.Address = "http://127.0.0.1"
	conf.HttpClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	return nil
}
