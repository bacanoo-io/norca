// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestOperatorSeal_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &OperatorSealStatusCommand{}
	var _ cli.Command = &OperatorUnsealCommand{}
	var _ cli.Command = &OperatorKeyringRewrapCommand{}
}

func TestOperatorSeal_RewrapStatusUnseal(t *testing.T) {
	ci.Parallel(t)
	srv, _, addr := testServer(t, false, nil)
	defer srv.Shutdown()

	// seal-status of a dev agent: implicit aead
	ui := cli.NewMockUi()
	code := (&OperatorSealStatusCommand{Meta: Meta{Ui: ui}}).Run([]string{"-address=" + addr})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "aead (implicit")

	// rewrap prints the shares once
	ui = cli.NewMockUi()
	code = (&OperatorKeyringRewrapCommand{Meta: Meta{Ui: ui}}).Run([]string{
		"-address=" + addr, "-to=shares", "-shares=3", "-threshold=2"})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
	shares := regexp.MustCompile(`Unseal share \d: (\S+)`).FindAllStringSubmatch(ui.OutputWriter.String(), -1)
	must.Len(t, 3, shares)
	must.StrContains(t, ui.ErrorWriter.String(), "shown only this once")

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		ui = cli.NewMockUi()
		code := (&OperatorSealStatusCommand{Meta: Meta{Ui: ui}}).Run([]string{"-address=" + addr})
		return code == 0 && strings.Contains(ui.OutputWriter.String(), "shares (2 of 3") &&
			!strings.Contains(ui.OutputWriter.String(), "Rewrap ")
	}), wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond)))

	// the share is never accepted as an argument
	ui = cli.NewMockUi()
	code = (&OperatorUnsealCommand{Meta: Meta{Ui: ui}}).Run([]string{"-address=" + addr, shares[0][1]})
	must.One(t, code)

	// from standard input
	ui = cli.NewMockUi()
	cmd := &OperatorUnsealCommand{Meta: Meta{Ui: ui}, testStdin: strings.NewReader(shares[0][1] + "\n")}
	code = cmd.Run([]string{"-address=" + addr})
	must.Zero(t, code, must.Sprint(ui.ErrorWriter.String()))
	must.StrContains(t, ui.OutputWriter.String(), "is unsealed")

	// -recover: an old share for a restored secret store, never as an
	// argument; this store needs no recovery
	ui = cli.NewMockUi()
	code = (&OperatorUnsealCommand{Meta: Meta{Ui: ui}}).Run([]string{"-address=" + addr, "-recover", shares[1][1]})
	must.One(t, code)
	ui = cli.NewMockUi()
	cmd = &OperatorUnsealCommand{Meta: Meta{Ui: ui}, testStdin: strings.NewReader(shares[1][1] + "\n")}
	code = cmd.Run([]string{"-address=" + addr, "-recover"})
	out := ui.OutputWriter.String() + ui.ErrorWriter.String()
	must.True(t, strings.Contains(out, "is recovered") || strings.Contains(out, "nothing to recover"), must.Sprint(out))
}
