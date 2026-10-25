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
	"github.com/hashicorp/nomad/api"
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
	var _ cli.Command = &OperatorBackupApproveCommand{}
	var _ cli.Command = &OperatorBackupRejectCommand{}
	var _ cli.Command = &OperatorBackupRestoresCommand{}
	var _ cli.Command = &OperatorBackupRestoreBootstrapCommand{}
}

func TestOperatorBackup_ParseBackupURL(t *testing.T) {
	ci.Parallel(t)

	bucket, prefix, id, err := parseBackupURL("s3://dr/norca/eu/2026/10/24/20261024T140000Z-1834567/")
	must.NoError(t, err)
	must.Eq(t, "dr", bucket)
	must.Eq(t, "norca/eu", prefix)
	must.Eq(t, "20261024T140000Z-1834567", id)

	// A backup at the root of the bucket.
	_, prefix, id, err = parseBackupURL("s3://dr/2026/10/24/20261024T140000Z-1834567")
	must.NoError(t, err)
	must.Eq(t, "", prefix)
	must.Eq(t, "20261024T140000Z-1834567", id)

	// A destination prefix: the latest backup.
	_, prefix, id, err = parseBackupURL("s3://dr/norca/eu/")
	must.NoError(t, err)
	must.Eq(t, "norca/eu", prefix)
	must.Eq(t, "", id)

	_, _, _, err = parseBackupURL("s3://dr/norca/2026/10/25/20261024T140000Z-1834567/")
	must.ErrorContains(t, err, "must end with")
	_, _, _, err = parseBackupURL("https://dr/norca/eu/")
	must.ErrorContains(t, err, "s3://")
}

// TestOperatorBackup_RestoreBootstrap builds a new single-server cluster
// from a backup of another one in the fake S3 store: the keyring first,
// then the state, so the Variables of the backup are readable.
func TestOperatorBackup_RestoreBootstrap(t *testing.T) {
	// Not parallel: the S3 credentials come from the environment.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "testsecret")

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	tmpDir := t.TempDir()
	identityFile := filepath.Join(tmpDir, "key.txt")
	must.NoError(t, os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600))

	serverConfig := func(dir string, withBackup bool) func(c *agent.Config) {
		return func(c *agent.Config) {
			c.DevMode = false
			c.DataDir = filepath.Join(tmpDir, dir)
			c.AdvertiseAddrs.HTTP = "127.0.0.1"
			c.AdvertiseAddrs.RPC = "127.0.0.1"
			c.AdvertiseAddrs.Serf = "127.0.0.1"
			if withBackup {
				c.Server.Backup = &agent.BackupConfig{
					Destinations: []*agent.BackupDestinationConfig{{
						Name: "primary", Type: "s3", Endpoint: s3.Endpoint(), Bucket: "backups", Region: "us-east-1",
						ForcePathStyle: pointer.Of(true), AccessKey: "test", SecretKey: "testsecret",
					}},
					Encryption: &agent.BackupEncryptionConfig{Recipients: []string{identity.Recipient().String()}},
				}
			}
		}
	}

	// The old cluster: a job and a Variables entry, backed up.
	oldSrv, oldClient, oldURL := testServer(t, false, serverConfig("old", true))
	defer oldSrv.Shutdown()
	testutil.WaitForLeader(t, oldSrv.Agent.RPC)
	job := testJob("bootstrapped")
	_, _, err = oldClient.Jobs().Register(job, nil)
	must.NoError(t, err)
	var saved *api.BackupSaveResponse
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			if _, _, err := oldClient.Variables().Create(&api.Variable{
				Path: "app/config", Items: api.VariableItems{"password": "s3cr3t"}}, nil); err != nil {
				return err
			}
			saved, _, err = oldClient.Backups().Save(nil, nil)
			return err
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(200*time.Millisecond),
	))
	from := "s3://backups/norca/global/" + saved.Backup.Path + "/"

	// The new cluster: a single, empty server.
	newSrv, newClient, newURL := testServer(t, false, serverConfig("new", false))
	defer newSrv.Shutdown()
	testutil.WaitForLeader(t, newSrv.Agent.RPC)

	run := func(url string, args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		cmd := &OperatorBackupRestoreBootstrapCommand{Meta: Meta{Ui: ui}}
		code := cmd.Run(append([]string{"-address=" + url, "-endpoint", s3.Endpoint(), "-force-path-style",
			"-s3-region", "us-east-1"}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	// The old cluster is not new; a wrong identity can't read the backup.
	code, _, errOut := run(oldURL, "-age-identity", identityFile, "-from", from)
	must.One(t, code)
	must.StrContains(t, errOut, "not new")
	other, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	otherFile := filepath.Join(tmpDir, "other.txt")
	must.NoError(t, os.WriteFile(otherFile, []byte(other.String()+"\n"), 0o600))
	code, _, errOut = run(newURL, "-age-identity", otherFile, "-from", from)
	must.One(t, code)
	must.StrContains(t, errOut, "does not match")

	// The latest backup of the destination prefix, into the new server.
	code, out, errOut := run(newURL, "-age-identity", identityFile, "-from", "s3://backups/norca/global/")
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, saved.ID)
	must.StrContains(t, out, "Restored 1 root keys (1 new)")

	restored, _, err := newClient.Jobs().Info(*job.ID, nil)
	must.NoError(t, err)
	must.Eq(t, *job.ID, *restored.ID)
	variable, _, err := newClient.Variables().Read("app/config", nil)
	must.NoError(t, err)
	must.Eq(t, "s3cr3t", variable.Items["password"])
}

func TestOperatorBackup_FullRestoreFlags(t *testing.T) {
	ci.Parallel(t)

	ui := cli.NewMockUi()
	cmd := &OperatorBackupRestoreCommand{Meta: Meta{Ui: ui}}
	must.One(t, cmd.Run([]string{"-full", "-age-identity", "key.txt", "20261024T140000Z-1"}))
	must.StrContains(t, ui.ErrorWriter.String(), "-confirm <region>")

	ui = cli.NewMockUi()
	cmd = &OperatorBackupRestoreCommand{Meta: Meta{Ui: ui}}
	must.One(t, cmd.Run([]string{"-full", "-job", "web", "-confirm", "global", "20261024T140000Z-1"}))
	must.StrContains(t, ui.ErrorWriter.String(), "takes no")

	ui = cli.NewMockUi()
	approve := &OperatorBackupApproveCommand{Meta: Meta{Ui: ui}}
	must.One(t, approve.Run([]string{"abc"}))
	must.StrContains(t, ui.ErrorWriter.String(), "-age-identity")
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

// TestOperatorBackup_RestoreSecret restores a secret from a backup as a new
// version with the CLI.
func TestOperatorBackup_RestoreSecret(t *testing.T) {
	ci.Parallel(t)
	const value = "canary-cli-7d1e-value"

	s3 := s3fake.New()
	defer s3.Close()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	tmpDir := t.TempDir()
	identityFile := filepath.Join(tmpDir, "key.txt")
	must.NoError(t, os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600))

	srv, client, url := testServer(t, false, func(c *agent.Config) {
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

	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			_, _, err := client.Secrets().Write("app/db", &api.SecretWrite{Items: map[string]string{"k": value}}, nil)
			return err
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(200*time.Millisecond),
	))
	var saved *api.BackupSaveResponse
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			saved, _, err = client.Backups().Save(nil, nil)
			return err
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(200*time.Millisecond),
	))
	_, _, err = client.Secrets().Write("app/db", &api.SecretWrite{Items: map[string]string{"k": "two"}}, nil)
	must.NoError(t, err)

	run := func(args ...string) (int, string, string) {
		ui := cli.NewMockUi()
		cmd := &OperatorBackupRestoreCommand{Meta: Meta{Ui: ui}}
		code := cmd.Run(append([]string{"-address=" + url, "-age-identity", identityFile}, args...))
		return code, ui.OutputWriter.String(), ui.ErrorWriter.String()
	}

	code, out, errOut := run("-secret", "app/db", saved.ID)
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "restored as version 3")
	must.StrNotContains(t, out, value)
	read, _, err := client.Secrets().Read("app/db", 0, nil)
	must.NoError(t, err)
	must.Eq(t, 3, read.Version)
	must.Eq(t, value, read.Items["k"])

	code, out, errOut = run("-dry-run", "-secret", "app/db", "-version", "1", "-with-metadata", saved.ID)
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, "secret default/app/db version 1")
	must.StrContains(t, out, "would update as version 4")

	code, out, errOut = run("-json", "-secret", "app/db@0", saved.ID)
	must.One(t, code)
	must.StrContains(t, out, "check index mismatch")

	code, out, _ = run("-secret", "app/db", "-version", "9", saved.ID)
	must.One(t, code)
	must.StrContains(t, out, "not in the backup")

	code, out, errOut = run("-json", "-secret", "app/db", saved.ID)
	must.Zero(t, code, must.Sprint(errOut))
	must.StrContains(t, out, `"NewVersion": 4`)
	must.StrContains(t, out, `"BackupVersion": 1`)
	must.StrNotContains(t, out, value)

	// Flag validation.
	code, _, errOut = run("-secret", "a", "-secret", "b", "-version", "1", saved.ID)
	must.One(t, code)
	must.StrContains(t, errOut, "single -secret")
	code, _, errOut = run("-job", "web", "-with-metadata", saved.ID)
	must.One(t, code)
	must.StrContains(t, errOut, "only used with -secret")
}
