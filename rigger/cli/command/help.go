// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/bacanoo-io/norca/rigger/module"
)

// The help of the program: the root says what Rigger is, each command what
// it does, with its flags and examples (rigger help <command>), and the
// topic "environment" the variables the commands read.

// rootLong is the help of the program: a header with the version, what it
// is, and how its command lines are written.
func (a *app) rootLong() string {
	return "Norca Rigger " + a.prog.Version + `
The package manager of Norca: it installs modules and stacks into a Norca
cluster, and writes, tests, signs and publishes them.

A flag is written --name; the one-dash form -name is read the same. Flags
may come before or after the arguments of a command. The environment the
commands read: ` + a.prog.Name + ` help environment.`
}

// environmentHelp is the help topic "environment".
const environmentHelp = `The commands read these variables of the environment.

The cluster:
  NORCA_ADDR, NORCA_TOKEN, NORCA_REGION, NORCA_NAMESPACE and the TLS
  settings of the norca command (NORCA_CACERT, ...): the cluster the
  commands talk to.
  RIGGER_SERVER=1      the stack commands plan, apply, status and destroy,
                       install and bundle import go to the servers of the
                       cluster, as with --server.

Values:
  RIGGER_VAR_<name>    the value of the variable <name> of a module or a
                       stack, read as a --var is: over the values of the
                       files of --values, under those of --var.

Sources and registries:
  RIGGER_GLOBAL_REGISTRY
                       the global registry: a git address or a directory
                       (default github.com/bacanoo-io/norca-global-registry).
                       A source <namespace>/<name> that names no registry of
                       the stack is a module of it.
  RIGGER_CACHE_DIR     the cache of fetched modules and registries (default:
                       rigger in the cache directory of the user).

Credentials (a credential goes to its host only, over https):
  RIGGER_CREDENTIALS_FILE
                       the file rigger login writes (default:
                       rigger/credentials.json in the configuration directory
                       of the user).
  RIGGER_GIT_USERNAME, RIGGER_GIT_PASSWORD
                       for the host RIGGER_GIT_HOST, when rigger login has
                       none for it.
  RIGGER_OCI_USERNAME, RIGGER_OCI_PASSWORD
                       for the host RIGGER_OCI_HOST, likewise.
  RIGGER_KEY_PASSWORD  the password of a private key (key generate, key sign,
                       module publish, stack publish); empty for a key
                       without a password.`

// newEnvironmentTopic is the help topic of the environment: a command that
// does not run, which "help environment" shows.
func newEnvironmentTopic() *cobra.Command {
	return &cobra.Command{
		Use:   "environment",
		Short: "The variables of the environment the commands read",
		Long:  environmentHelp,
	}
}

// usageTemplate is cobra's usage template with the help command named in
// its last line: "norca rigger stack --help" reaches norca, not Rigger.
func usageTemplate(cobraTemplate string) string {
	return strings.Replace(cobraTemplate,
		`Use "{{.CommandPath}} [command] --help" for more information about a command.`,
		`Use "{{.Root.CommandPath}} help <command>" for more information about a command.`, 1)
}

// newGroup returns a command that holds commands: run without one, or with
// one it does not hold, it fails, naming them in the order given.
func newGroup(c *cobra.Command, sub ...*cobra.Command) *cobra.Command {
	names := make([]string, 0, len(sub))
	for _, s := range sub {
		names = append(names, s.Name())
	}

	c.Args = cobra.ArbitraryArgs
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("%s needs a command: %s", cmd.Name(), orList(names))
		}

		return fmt.Errorf("unknown %s command %q", cmd.Name(), args[0])
	}

	c.AddCommand(sub...)

	return c
}

// orList joins words as "a, b or c".
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}

	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// showOnly hides in the help and the completion of cmd its flags but
// those named: the commands of a group parse the flags they always took,
// and show those that do something for them.
func showOnly(cmd *cobra.Command, names ...string) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !slices.Contains(names, f.Name) {
			f.Hidden = true
		}
	})
}

// The values a flag takes from a fixed set, completed by the shell.
var (
	kindValues      = []string{"module", "stack"}
	logFormatValues = []string{"text", "json"}
	logLevelValues  = []string{"debug", "info", "warn", "error"}
	searchRegistry  = []string{searchGlobal, searchBuiltin}
)

// registerCompletions completes the values of the flags that take one of a
// fixed set. Only a run that completes needs them (isCompletion): cobra
// keeps them in a map of the process.
func registerCompletions(root *cobra.Command) {
	fixed := func(values []string) cobra.CompletionFunc {
		return cobra.FixedCompletions(values, cobra.ShellCompDirectiveNoFileComp)
	}

	// The command, by its path under the root, its flag and its values.
	flags := []struct {
		path   []string
		flag   string
		values []string
	}{
		{nil, logFormatFlag, logFormatValues},
		{nil, logLevelFlag, logLevelValues},
		{[]string{cmdSearch}, "kind", kindValues},
		{[]string{cmdSearch}, "registry", searchRegistry},
		{[]string{cmdSearch}, "category", module.Categories},
		{[]string{cmdShow}, "kind", kindValues},
		{[]string{cmdRegistry, cmdRegistrySearch}, "kind", kindValues},
		{[]string{cmdRegistry, cmdRegistrySearch}, "category", module.Categories},
	}

	for _, f := range flags {
		cmd, _, err := root.Find(f.path)
		if err != nil || cmd == nil {
			continue
		}

		// The flags exist: a name that does not is a mistake of this table,
		// which the tests of the completion find.
		_ = cmd.RegisterFlagCompletionFunc(f.flag, fixed(f.values))
	}
}

// isCompletion reports whether the arguments of a run complete a command
// line: cobra's hidden commands __complete and __completeNoDesc.
func isCompletion(args []string) bool {
	return len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd)
}
