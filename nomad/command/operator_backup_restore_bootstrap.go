// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/posener/complete"
)

type OperatorBackupRestoreBootstrapCommand struct {
	Meta
}

func (c *OperatorBackupRestoreBootstrapCommand) Name() string {
	return "operator backup restore-bootstrap"
}

func (c *OperatorBackupRestoreBootstrapCommand) Synopsis() string {
	return "Build a new cluster from a backup"
}

func (c *OperatorBackupRestoreBootstrapCommand) Help() string {
	helpText := `
Usage: norca operator backup restore-bootstrap [options] -from <s3 url>

  Build a new cluster from a backup, for disaster recovery or a migration to
  another cloud. The backup is read from the S3-compatible store and
  decrypted on the machine running this command, with the age identity of the
  administrators; neither the identity nor the S3 credentials are sent to the
  cluster.

  Start a single new server first, with the region of the backup in its
  configuration and bootstrap_expect = 1, and bootstrap its ACLs if they are
  enabled. Then run this command against it with that management token. The
  command writes the keys of the backup into the server's keystore, then
  restores the Raft state of the backup. Afterwards the ACL tokens of the
  backup are in effect, and the other new servers can join; they replicate
  the state and the keys from the first one.

  -from is the folder of a backup, for example
  s3://norca-backup-dr/norca/eu-aws/2026/10/24/20261024T140000Z-1834567/, or
  the prefix of a destination (s3://norca-backup-dr/norca/eu-aws/) to restore
  the latest backup. The S3 credentials are read from the environment
  (AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or MINIO_ACCESS_KEY and
  MINIO_SECRET_KEY), the shared credentials file or the instance profile.

  The region of the backup must match the region of the server; the region
  references of the jobs are not rewritten. The cluster must be new: one
  server and no jobs, unless -force is given.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Restore Bootstrap Options:
` + backupIdentityUsage + `

  -from <url>
    The s3:// URL of the backup folder, or of a destination prefix for the
    latest backup.

  -endpoint <url>
    The S3-compatible endpoint, for example https://s3.example.com. Empty
    means AWS S3.

  -s3-region <region>
    The region of the bucket.

  -force-path-style
    Use path-style bucket addressing, as most S3-compatible stores need.

  -credentials-file <path>
    An AWS shared credentials file, with -profile.

  -profile <name>
    The profile of the shared credentials file.

  -ca-file <path>
    A PEM bundle of CAs to trust for the endpoint.

  -allow-region-change
    Restore a backup of another region. The region references of the jobs
    are not rewritten.

  -force
    Restore into a cluster that is not new: every change of its state is
    lost.
`
	return strings.TrimSpace(helpText)
}

func (c *OperatorBackupRestoreBootstrapCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-age-identity":        complete.PredictFiles("*"),
			"-from":                complete.PredictAnything,
			"-endpoint":            complete.PredictAnything,
			"-s3-region":           complete.PredictAnything,
			"-force-path-style":    complete.PredictNothing,
			"-credentials-file":    complete.PredictFiles("*"),
			"-profile":             complete.PredictAnything,
			"-ca-file":             complete.PredictFiles("*"),
			"-allow-region-change": complete.PredictNothing,
			"-force":               complete.PredictNothing,
		})
}

func (c *OperatorBackupRestoreBootstrapCommand) AutocompleteArgs() complete.Predictor {
	return complete.PredictNothing
}

// parseBackupURL splits an s3:// URL into the bucket, the destination
// prefix and the backup ID; the ID is empty for a destination prefix.
func parseBackupURL(raw string) (bucket, prefix, id string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid -from URL: %w", err)
	}
	if u.Scheme != "s3" || u.Host == "" {
		return "", "", "", fmt.Errorf("-from must be an s3://<bucket>/<path> URL")
	}
	p := strings.Trim(u.Path, "/")
	last := path.Base(p)
	if _, _, perr := backup.ParseID(last); p != "" && perr == nil {
		folder, _ := backup.PathFor(last)
		if p != folder && !strings.HasSuffix(p, "/"+folder) {
			return "", "", "", fmt.Errorf("backup folder %q must end with %s", p, folder)
		}
		return u.Host, strings.TrimSuffix(strings.TrimSuffix(p, folder), "/"), last, nil
	}
	return u.Host, p, "", nil
}

func (c *OperatorBackupRestoreBootstrapCommand) Run(args []string) int {
	var identityPath, from string
	var allowRegionChange, force bool
	dcfg := &backup.DestinationConfig{Name: "from", Type: backup.DestinationTypeS3}

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&identityPath, "age-identity", "", "")
	flags.StringVar(&from, "from", "", "")
	flags.StringVar(&dcfg.Endpoint, "endpoint", "", "")
	flags.StringVar(&dcfg.Region, "s3-region", "", "")
	flags.BoolVar(&dcfg.ForcePathStyle, "force-path-style", false, "")
	flags.StringVar(&dcfg.CredentialsFile, "credentials-file", "", "")
	flags.StringVar(&dcfg.Profile, "profile", "", "")
	flags.StringVar(&dcfg.CAFile, "ca-file", "", "")
	flags.BoolVar(&allowRegionChange, "allow-region-change", false, "")
	flags.BoolVar(&force, "force", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if from == "" {
		c.Ui.Error("The backup to restore is required: use -from s3://<bucket>/<path>")
		return 1
	}

	bucket, prefix, id, err := parseBackupURL(from)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	identity, err := readAgeIdentity(identityPath)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	ids, err := backup.ParseIdentities(identity)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	dcfg.Bucket = bucket
	// An empty prefix would select the default one; "/" is the bucket root.
	dcfg.Prefix = prefix
	if dcfg.Prefix == "" {
		dcfg.Prefix = "/"
	}
	if err := dcfg.Validate(); err != nil {
		c.Ui.Error(fmt.Sprintf("Invalid S3 options: %s", err))
		return 1
	}
	dest, err := backup.NewDestination(dcfg, backup.DefaultConfig(), "")
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error creating the S3 client: %s", err))
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}
	region := c.Meta.region
	if region == "" {
		if region, err = client.Agent().Region(); err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading the region of the server: %s", err))
			return 1
		}
	}

	ctx := context.Background()
	if id == "" {
		if id, err = backup.ReadLatest(ctx, dest); err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading the latest backup of %s: %s", from, err))
			return 1
		}
	}
	record, err := backup.ReadRecord(ctx, dest, id)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			err = fmt.Errorf("backup %s not found", id)
		}
		c.Ui.Error(fmt.Sprintf("Error reading the manifest: %s", err))
		return 1
	}
	if err := backup.CheckRestorable(record, region, allowRegionChange); err != nil {
		if record.Region != region && !allowRegionChange {
			c.Ui.Error(fmt.Sprintf("%s; use -allow-region-change to restore it anyway", err))
		} else {
			c.Ui.Error(err.Error())
		}
		return 1
	}

	if !force {
		if err := checkNewCluster(client); err != nil {
			c.Ui.Error(fmt.Sprintf("%s; use -force to restore into it anyway", err))
			return 1
		}
	}

	// Download and verify everything before the cluster is touched.
	archive, err := os.CreateTemp("", "norca-restore-*.snap")
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	st := record.Destinations[0]
	size, err := backup.DownloadState(ctx, dest, record, st, ids, archive)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	keys, err := backup.ReadKeys(ctx, dest, record, st, ids)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Downloaded and verified backup %s (Raft index %d, %d bytes)", record.ID, record.RaftIndex, size))

	// 1. The keystore.
	if len(keys) > 0 {
		resp, _, err := client.Keyring().Restore(apiRootKeys(keys), nil)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error restoring the keyring: %s", err))
			return 1
		}
		c.Ui.Output(fmt.Sprintf("Restored %d root keys (%d new) on %s", len(keys), len(resp.Added),
			strings.Join(resp.Servers, ",")))
		for name, e := range resp.Errors {
			c.Ui.Warn(fmt.Sprintf("Root keys not restored on %s: %s", name, e))
		}
	} else {
		c.Ui.Warn("The backup has no keyring: its Variables can't be decrypted")
	}

	// 2. The Raft state.
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}
	if _, err := client.Operator().SnapshotRestore(archive, nil); err != nil {
		c.Ui.Error(fmt.Sprintf("Error restoring the state: %s", err))
		return 1
	}
	c.Ui.Output(fmt.Sprintf("Restored the state of backup %s into region %q.", record.ID, region))
	c.Ui.Output("The ACL tokens of the backup are in effect now. Join the other new servers to this one.")
	return 0
}

// checkNewCluster returns an error unless the cluster has a single server
// and no jobs.
func checkNewCluster(client *api.Client) error {
	raft, err := client.Operator().RaftGetConfiguration(nil)
	if err != nil {
		return fmt.Errorf("failed to read the Raft configuration: %w", err)
	}
	if len(raft.Servers) > 1 {
		return fmt.Errorf("the cluster has %d servers, not a single new one", len(raft.Servers))
	}
	jobs, _, err := client.Jobs().List(&api.QueryOptions{Namespace: "*"})
	if err != nil {
		return fmt.Errorf("failed to list the jobs: %w", err)
	}
	if len(jobs) > 0 {
		return fmt.Errorf("the cluster is not new: it has %d jobs", len(jobs))
	}
	return nil
}

// apiRootKeys converts root keys for the HTTP API.
func apiRootKeys(keys []*structs.RootKey) []*api.RootKey {
	out := make([]*api.RootKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, &api.RootKey{
			Meta: &api.RootKeyMeta{
				KeyID:       k.Meta.KeyID,
				Algorithm:   api.EncryptionAlgorithm(k.Meta.Algorithm),
				CreateTime:  k.Meta.CreateTime,
				CreateIndex: k.Meta.CreateIndex,
				ModifyIndex: k.Meta.ModifyIndex,
				State:       api.RootKeyState(k.Meta.State),
			},
			Key: k.Key,
		})
	}
	return out
}
