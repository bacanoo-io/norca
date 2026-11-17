// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
)

// TestCDCConsumerCommands_Arguments checks that the consumer commands, which
// name their stream with -stream, refuse positional arguments before any
// request is sent: "cdc consumer delete kv" must not delete the consumer of
// the default stream.
func TestCDCConsumerCommands_Arguments(t *testing.T) {
	ci.Parallel(t)

	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	sent := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}

	commands := map[string]func(Meta) cli.Command{
		"delete": func(m Meta) cli.Command { return &CDCConsumerDeleteCommand{Meta: m} },
		"read":   func(m Meta) cli.Command { return &CDCConsumerReadCommand{Meta: m} },
	}
	for name, mk := range commands {
		ui := cli.NewMockUi()
		code := mk(Meta{Ui: ui}).Run([]string{"-address=" + srv.URL, "kv"})
		must.One(t, code, must.Sprint(name, ui.OutputWriter.String()))
		must.StrContains(t, ui.ErrorWriter.String(), "This command takes no arguments", must.Sprint(name))
	}
	must.SliceEmpty(t, sent())

	// The stream named with -stream is the one addressed.
	ui := cli.NewMockUi()
	code := (&CDCConsumerDeleteCommand{Meta: Meta{Ui: ui}}).Run([]string{"-address=" + srv.URL, "-stream", "kv"})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
	must.Eq(t, []string{"DELETE /v1/cdc/consumer/kv"}, sent())
}
