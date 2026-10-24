// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/testutil"
	"github.com/mitchellh/cli"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

func TestOperatorBackupCommands_Implement(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &OperatorBackupCommand{}
	var _ cli.Command = &OperatorBackupListCommand{}
	var _ cli.Command = &OperatorBackupInspectCommand{}
	var _ cli.Command = &OperatorBackupContentsCommand{}
	var _ cli.Command = &OperatorBackupDiffCommand{}
	var _ cli.Command = &OperatorBackupSaveCommand{}
	var _ cli.Command = &OperatorBackupPinCommand{}
	var _ cli.Command = &OperatorBackupDeleteCommand{}
	var _ cli.Command = &OperatorBackupRestoreCommand{}
	var _ cli.Command = &OperatorBackupDownloadCommand{}
}

func TestOperatorBackup_Helpers(t *testing.T) {
	ci.Parallel(t)

	labels, err := parseBackupLabels([]string{"note=before upgrade", "ticket=OPS-1"})
	must.NoError(t, err)
	must.Eq(t, map[string]string{"note": "before upgrade", "ticket": "OPS-1"}, labels)
	_, err = parseBackupLabels([]string{"novalue"})
	must.Error(t, err)

	name, n, err := splitAt("web@3")
	must.NoError(t, err)
	must.Eq(t, "web", name)
	must.Eq(t, uint64(3), *n)
	name, n, err = splitAt("nomad/jobs/web")
	must.NoError(t, err)
	must.Eq(t, "nomad/jobs/web", name)
	must.Nil(t, n)
	_, _, err = splitAt("web@x")
	must.Error(t, err)

	_, err = readAgeIdentity("")
	must.ErrorContains(t, err, "-age-identity")
}

func TestOperatorBackup_EndToEnd(t *testing.T) {
	ci.Parallel(t)

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	tmpDir := t.TempDir()
	identityFile := filepath.Join(tmpDir, "key.txt")
	must.NoError(t, os.WriteFile(identityFile, []byte("# test key\n"+identity.String()+"\n"), 0o600))

	srv, _, url := testServer(t, false, func(c *agent.Config) {
		c.DevMode = false
		c.DataDir = filepath.Join(tmpDir, "server")
		c.AdvertiseAddrs.HTTP = "127.0.0.1"
		c.AdvertiseAddrs.RPC = "127.0.0.1"
		c.AdvertiseAddrs.Serf = "127.0.0.1"
		c.Server.Backup = &agent.BackupConfig{
			Destinations: []*agent.BackupDestinationConfig{{
				Name: "primary", Type: "s3", Endpoint: s3.Endpoint(), Bucket: "backups", Region: "us-east-1",
				ForcePathStyle: pointer.Of(true), AccessKey: "test", SecretKey: "testsecret",
			}},
			Encryption: &agent.BackupEncryptionConfig{Recipients: []string{identity.Recipient().String()}},
		}
	})
	defer srv.Shutdown()
	testutil.WaitForLeader(t, srv.Agent.RPC)

	run := func(cmd cli.Command, args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		switch c := cmd.(type) {
		case *OperatorBackupSaveCommand:
			c.Meta = Meta{Ui: ui}
		case *OperatorBackupListCommand:
			c.Meta = Meta{Ui: ui}
		case *OperatorBackupContentsCommand:
			c.Meta = Meta{Ui: ui}
		case *OperatorBackupRestoreCommand:
			c.Meta = Meta{Ui: ui}
		case *OperatorBackupPinCommand:
			c.Meta = Meta{Ui: ui}
		}
		code := cmd.Run(append([]string{"-address=" + url}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	// Save (retried until the keyring is initialized).
	var out string
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			code, o, _ := run(&OperatorBackupSaveCommand{}, "-label", "note=cli")
			out = o
			return code == 0
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(200*time.Millisecond),
	))
	id := regexp.MustCompile(`Backup "([^"]+)" complete`).FindStringSubmatch(out)
	must.Len(t, 2, id, must.Sprint(out))

	code, out, errOut := run(&OperatorBackupListCommand{})
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, id[1])
	must.StrContains(t, out, "note=cli")

	code, out, errOut = run(&OperatorBackupContentsCommand{}, "-age-identity", identityFile, "-type", "namespaces", id[1])
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "default")

	code, _, errOut = run(&OperatorBackupContentsCommand{}, id[1])
	must.One(t, code)
	must.StrContains(t, errOut, "-age-identity")

	code, out, errOut = run(&OperatorBackupRestoreCommand{}, "-age-identity", identityFile, "-dry-run",
		"-restore-namespace", "default", id[1])
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "would update")

	code, out, errOut = run(&OperatorBackupPinCommand{}, id[1])
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "pinned=true")
}
