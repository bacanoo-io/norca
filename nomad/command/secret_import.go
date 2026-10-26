// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/posener/complete"

	"github.com/hashicorp/nomad/api"
	flaghelper "github.com/hashicorp/nomad/helper/flags"
)

type SecretImportCommand struct {
	Meta

	// vaultClient replaces the Vault client of the environment in tests.
	vaultClient *vaultapi.Client
}

func (c *SecretImportCommand) Name() string { return "secret import" }

func (c *SecretImportCommand) Synopsis() string {
	return "Import secrets from Vault KV or from Variables"
}

func (c *SecretImportCommand) Help() string {
	helpText := `
Usage: norca secret import [options] -from-vault <mount>/<prefix>
       norca secret import [options] -from-variable <path>

  Copy secrets into the built-in secret store of the namespace given with
  -namespace.

  -from-vault reads the KV secrets below a prefix of a Vault (or OpenBao) KV
  v1 or v2 mount, recursively, with the standard Vault environment of the
  user: VAULT_ADDR, VAULT_TOKEN (or the token file of the vault CLI,
  ~/.vault-token), VAULT_NAMESPACE, VAULT_CACERT and the other VAULT_*
  variables. Each secret is written to the path below the mount (kv/app/db
  becomes app/db), the same path that Vault-style lookups of jobs read
  through the default vault_kv rules once the agents' vault blocks are
  removed. Values that are not strings are stored as JSON. Only the latest
  version is copied; deleted versions are skipped.

  -from-variable copies a Variable (or, for a path ending in "/", every
  Variable below it) into a secret of the same path; -move deletes the
  Variable after it was copied.

  Values are never printed: the command prints the source and target paths
  and versions of each secret, and a summary.

  If ACLs are enabled, this command requires the 'write-secret' capability
  (and 'list-secrets' for -dry-run) for the target paths, and 'variables'
  read (and destroy for -move) for -from-variable.

General Options:

  ` + generalOptionsUsage(usageOptsDefault) + `

Import Options:

  -from-vault <mount>/<prefix>
    The Vault KV mount and the prefix below it to import, for example
    "kv/app/" or "secret/app/db". A KV v2 prefix may include "data/".

  -from-variable <path>
    The Variable, or with a trailing "/" the Variables below the path, to
    import.

  -kv-version <1|2>
    The KV version of the Vault mount; by default it is read from Vault.

  -vault-address <address>, -vault-namespace <namespace>
    Override VAULT_ADDR and VAULT_NAMESPACE.

  -prefix-map <from>=<to>
    Write the secrets whose source path (below the mount for Vault) starts
    with <from> to <to> followed by the rest of the path; can be repeated,
    the longest <from> wins. For example -prefix-map app/=prod/app/.

  -overwrite
    Write a new version of secrets that exist already; by default they are
    skipped.

  -move
    With -from-variable, delete each Variable once it was copied.

  -dry-run
    Show what would be imported without writing anything. KV v2 values are
    not read (the versions come from the metadata).
`
	return strings.TrimSpace(helpText)
}

func (c *SecretImportCommand) AutocompleteFlags() complete.Flags {
	return mergeAutocompleteFlags(c.Meta.AutocompleteFlags(FlagSetClient),
		complete.Flags{
			"-from-vault":      complete.PredictAnything,
			"-from-variable":   complete.PredictAnything,
			"-kv-version":      complete.PredictSet("1", "2"),
			"-vault-address":   complete.PredictAnything,
			"-vault-namespace": complete.PredictAnything,
			"-prefix-map":      complete.PredictAnything,
			"-overwrite":       complete.PredictNothing,
			"-move":            complete.PredictNothing,
			"-dry-run":         complete.PredictNothing,
		})
}

func (c *SecretImportCommand) AutocompleteArgs() complete.Predictor { return complete.PredictNothing }

// importItem is one secret to import.
type importItem struct {
	source string // shown source path
	rel    string // path the prefix map applies to
	target string
	// version is the source version, "" if the source has none.
	version string

	// read reads the items of the source; nil items skip the secret with
	// skipReason.
	read func() (items map[string]string, skipReason string, err error)

	// after runs once the secret was written (-move).
	after func() error
}

func (c *SecretImportCommand) Run(args []string) int {
	var fromVault, fromVariable, vaultAddr, vaultNamespace string
	var kvVersion int
	var overwrite, move, dryRun bool
	var prefixMaps flaghelper.StringFlag

	flags := c.Meta.FlagSet(c.Name(), FlagSetClient)
	flags.Usage = func() { c.Ui.Output(c.Help()) }
	flags.StringVar(&fromVault, "from-vault", "", "")
	flags.StringVar(&fromVariable, "from-variable", "", "")
	flags.IntVar(&kvVersion, "kv-version", 0, "")
	flags.StringVar(&vaultAddr, "vault-address", "", "")
	flags.StringVar(&vaultNamespace, "vault-namespace", "", "")
	flags.Var(&prefixMaps, "prefix-map", "")
	flags.BoolVar(&overwrite, "overwrite", false, "")
	flags.BoolVar(&move, "move", false, "")
	flags.BoolVar(&dryRun, "dry-run", false, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 0 {
		c.Ui.Error("This command takes no arguments")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if (fromVault == "") == (fromVariable == "") {
		c.Ui.Error("Exactly one of -from-vault and -from-variable is required")
		c.Ui.Error(commandErrorText(c))
		return 1
	}
	if move && fromVariable == "" {
		c.Ui.Error("-move is only supported with -from-variable")
		return 1
	}
	if kvVersion != 0 && kvVersion != 1 && kvVersion != 2 {
		c.Ui.Error("-kv-version must be 1 or 2")
		return 1
	}
	mapper, err := parsePrefixMaps(prefixMaps)
	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	client, err := c.Meta.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error initializing client: %s", err))
		return 1
	}

	var items []*importItem
	if fromVault != "" {
		vc, err := c.vault(vaultAddr, vaultNamespace)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error initializing the Vault client: %s", err))
			return 1
		}
		items, err = vaultImportItems(vc, fromVault, kvVersion, dryRun)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading from Vault: %s", err))
			return 1
		}
	} else {
		items, err = variableImportItems(client, fromVariable, move)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error reading Variables: %s", err))
			return 1
		}
	}
	if len(items) == 0 {
		c.Ui.Output("No secrets found to import")
		return 0
	}
	for _, it := range items {
		it.target = mapper(it.rel)
	}

	var imported, skipped, failed int
	for _, it := range items {
		src := it.source
		if it.version != "" {
			src += " (version " + it.version + ")"
		}
		existing, _, err := client.Secrets().Info(it.target, nil)
		exists := err == nil && !existing.Destroyed
		if err != nil && !errors.Is(err, api.ErrSecretNotFound) {
			c.Ui.Error(fmt.Sprintf("%s -> %s: failed to read the target: %s", src, it.target, err))
			failed++
			continue
		}
		if exists && !overwrite {
			c.Ui.Output(fmt.Sprintf("%s -> %s: skipped, the secret exists (version %d); use -overwrite to write a new version",
				src, it.target, existing.CurrentVersion))
			skipped++
			continue
		}
		if dryRun {
			action := "would import"
			if exists {
				action = fmt.Sprintf("would write a new version over version %d", existing.CurrentVersion)
			}
			c.Ui.Output(fmt.Sprintf("%s -> %s: %s", src, it.target, action))
			imported++
			continue
		}

		values, skip, err := it.read()
		if err != nil {
			c.Ui.Error(fmt.Sprintf("%s -> %s: failed to read the source: %s", src, it.target, err))
			failed++
			continue
		}
		if values == nil {
			c.Ui.Output(fmt.Sprintf("%s -> %s: skipped, %s", src, it.target, skip))
			skipped++
			continue
		}
		write := &api.SecretWrite{Items: values}
		var secret *api.Secret
		if exists {
			secret, _, err = client.Secrets().Write(it.target, write, nil)
		} else {
			// Never overwrite a secret written in the meantime.
			secret, _, err = client.Secrets().CheckedWrite(it.target, 0, write, nil)
		}
		if err != nil {
			c.Ui.Error(fmt.Sprintf("%s -> %s: failed to write the secret: %s", src, it.target, err))
			failed++
			continue
		}
		msg := fmt.Sprintf("%s -> %s: imported as version %d", src, it.target, secret.CurrentVersion)
		if it.after != nil {
			if err := it.after(); err != nil {
				c.Ui.Error(fmt.Sprintf("%s, but failed to delete the source: %s", msg, err))
				failed++
				continue
			}
			msg += ", source deleted"
		}
		c.Ui.Output(msg)
		imported++
	}

	verb := "Imported"
	if dryRun {
		verb = "Would import"
	}
	c.Ui.Output(fmt.Sprintf("\n%s %d, skipped %d, failed %d of %d secrets", verb, imported, skipped, failed, len(items)))
	if failed > 0 {
		return 1
	}
	return 0
}

// parsePrefixMaps returns the mapping of source paths to target paths.
func parsePrefixMaps(maps []string) (func(string) string, error) {
	type rule struct{ from, to string }
	var rules []rule
	for _, m := range maps {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" {
			return nil, fmt.Errorf("invalid -prefix-map %q, must be <from>=<to>", m)
		}
		rules = append(rules, rule{strings.TrimPrefix(from, "/"), strings.TrimPrefix(to, "/")})
	}
	sort.SliceStable(rules, func(i, j int) bool { return len(rules[i].from) > len(rules[j].from) })
	return func(p string) string {
		for _, r := range rules {
			if rest, ok := strings.CutPrefix(p, r.from); ok {
				return r.to + rest
			}
		}
		return p
	}, nil
}

// vault returns the Vault client of the user's environment.
func (c *SecretImportCommand) vault(addr, namespace string) (*vaultapi.Client, error) {
	if c.vaultClient != nil {
		return c.vaultClient, nil
	}
	conf := vaultapi.DefaultConfig()
	if conf.Error != nil {
		return nil, conf.Error
	}
	if addr != "" {
		conf.Address = addr
	}
	vc, err := vaultapi.NewClient(conf)
	if err != nil {
		return nil, err
	}
	if namespace != "" {
		vc.SetNamespace(namespace)
	}
	if vc.Token() == "" {
		// The token file of the vault CLI's default token helper.
		if home, err := os.UserHomeDir(); err == nil {
			if b, err := os.ReadFile(filepath.Join(home, ".vault-token")); err == nil {
				vc.SetToken(strings.TrimSpace(string(b)))
			}
		}
	}
	if vc.Token() == "" {
		return nil, errors.New("no Vault token: set VAULT_TOKEN or log in with the vault CLI")
	}
	return vc, nil
}

// vaultSource is a Vault KV mount and a prefix below it.
type vaultSource struct {
	vc      *vaultapi.Client
	mount   string // with a trailing "/"
	version int
}

// vaultImportItems lists the secrets below a Vault KV prefix.
func vaultImportItems(vc *vaultapi.Client, from string, kvVersion int, dryRun bool) ([]*importItem, error) {
	from = strings.TrimPrefix(from, "/")
	if from == "" {
		return nil, errors.New("-from-vault needs <mount>/<prefix>")
	}
	src := &vaultSource{vc: vc, version: kvVersion}

	// The mount and its KV version: from Vault's mount lookup, which every
	// token may use for the paths it can access, else the first segment.
	if sec, err := vc.Logical().Read("sys/internal/ui/mounts/" + from); err == nil && sec != nil {
		if p, ok := sec.Data["path"].(string); ok && p != "" {
			src.mount = strings.TrimPrefix(p, "/")
		}
		if t, ok := sec.Data["type"].(string); ok && t != "" && t != "kv" && t != "generic" {
			return nil, fmt.Errorf("the mount %q is a %s secrets engine; only KV secrets can be imported", src.mount, t)
		}
		if opts, ok := sec.Data["options"].(map[string]any); ok && src.version == 0 {
			if v, ok := opts["version"].(string); ok {
				src.version, _ = strconv.Atoi(v)
			}
		}
	}
	if src.mount == "" {
		first, _, _ := strings.Cut(from, "/")
		src.mount = first + "/"
	}
	if src.version == 0 {
		src.version = 1
	}
	prefix := strings.TrimPrefix(from, src.mount)
	if strings.TrimSuffix(from, "/")+"/" == src.mount {
		prefix = ""
	}
	if src.version == 2 {
		prefix = strings.TrimPrefix(prefix, "data/")
	}

	var items []*importItem
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		// A single secret, and the secrets below it if it is a directory too.
		if it, err := src.item(prefix, dryRun); err != nil {
			return nil, err
		} else if it != nil {
			items = append(items, it)
		}
		prefix += "/"
	}
	below, err := src.walk(prefix, dryRun)
	if err != nil {
		return nil, err
	}
	return append(items, below...), nil
}

func (s *vaultSource) dataPath(p string) string {
	if s.version == 2 {
		return s.mount + "data/" + p
	}
	return s.mount + p
}

func (s *vaultSource) metadataPath(p string) string {
	if s.version == 2 {
		return s.mount + "metadata/" + p
	}
	return s.mount + p
}

// walk lists the secrets below a directory, recursively.
func (s *vaultSource) walk(dir string, dryRun bool) ([]*importItem, error) {
	sec, err := s.vc.Logical().List(s.metadataPath(dir))
	if err != nil {
		return nil, err
	}
	if sec == nil {
		return nil, nil
	}
	raw, _ := sec.Data["keys"].([]any)
	keys := make([]string, 0, len(raw))
	for _, k := range raw {
		if ks, ok := k.(string); ok {
			keys = append(keys, ks)
		}
	}
	sort.Strings(keys)
	var out []*importItem
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			below, err := s.walk(dir+k, dryRun)
			if err != nil {
				return nil, err
			}
			out = append(out, below...)
			continue
		}
		it, err := s.item(dir+k, dryRun)
		if err != nil {
			return nil, err
		}
		if it != nil {
			out = append(out, it)
		}
	}
	return out, nil
}

// item returns the import of one secret, nil if there is none at the path.
// For KV v2 the version comes from the metadata, so a dry run never reads
// values.
func (s *vaultSource) item(p string, dryRun bool) (*importItem, error) {
	it := &importItem{source: s.dataPath(p), rel: p}
	if s.version == 2 {
		md, err := s.vc.Logical().Read(s.metadataPath(p))
		if err != nil {
			return nil, err
		}
		if md == nil {
			return nil, nil
		}
		if v, ok := md.Data["current_version"].(json.Number); ok {
			it.version = v.String()
		}
	} else if dryRun {
		// KV v1 has no metadata; check that the secret exists.
		sec, err := s.vc.Logical().Read(s.dataPath(p))
		if err != nil {
			return nil, err
		}
		if sec == nil {
			return nil, nil
		}
	}
	it.read = func() (map[string]string, string, error) {
		sec, err := s.vc.Logical().Read(s.dataPath(p))
		if err != nil {
			return nil, "", err
		}
		if sec == nil || sec.Data == nil {
			return nil, "the secret does not exist in Vault", nil
		}
		data := sec.Data
		if s.version == 2 {
			inner, ok := data["data"].(map[string]any)
			if !ok {
				return nil, "the latest version is deleted in Vault", nil
			}
			data = inner
		}
		items := make(map[string]string, len(data))
		for k, v := range data {
			switch t := v.(type) {
			case string:
				items[k] = t
			case nil:
				items[k] = ""
			default:
				b, err := json.Marshal(t)
				if err != nil {
					return nil, "", fmt.Errorf("item %q can't be encoded", k)
				}
				items[k] = string(b)
			}
		}
		if len(items) == 0 {
			return nil, "the secret has no items", nil
		}
		return items, "", nil
	}
	return it, nil
}

// variableImportItems lists the Variables to import: one path, or every
// Variable below a path ending in "/".
func variableImportItems(client *api.Client, from string, move bool) ([]*importItem, error) {
	from = strings.TrimPrefix(from, "/")
	var paths []string
	if strings.HasSuffix(from, "/") || from == "" {
		list, _, err := client.Variables().PrefixList(from, nil)
		if err != nil {
			return nil, err
		}
		for _, v := range list {
			paths = append(paths, v.Path)
		}
		sort.Strings(paths)
	} else {
		paths = []string{from}
	}
	out := make([]*importItem, 0, len(paths))
	for _, p := range paths {
		p := p
		var modifyIndex uint64
		it := &importItem{source: "var " + p, rel: p}
		it.read = func() (map[string]string, string, error) {
			v, _, err := client.Variables().Read(p, nil)
			if errors.Is(err, api.ErrVariablePathNotFound) {
				return nil, "the Variable does not exist", nil
			}
			if err != nil {
				return nil, "", err
			}
			if len(v.Items) == 0 {
				return nil, "the Variable has no items", nil
			}
			modifyIndex = v.ModifyIndex
			return map[string]string(v.Items), "", nil
		}
		if move {
			it.after = func() error {
				_, err := client.Variables().CheckedDelete(p, modifyIndex, nil)
				return err
			}
		}
		out = append(out, it)
	}
	return out, nil
}
