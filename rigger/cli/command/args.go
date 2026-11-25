// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// normalizeArgs returns the arguments of a run with each long flag written
// with one dash, -name or -name=value, written --name or --name=value, as
// pflag reads it. The flag package that read the command lines before took
// one dash, and the scripts, the deployer and the documentation use it.
//
// The arguments are read as cobra finds the command (Command.Find): a word
// that names a command of the current one, before any other word, is that
// command, and its flags, with those its parents pass down, are the ones
// rewritten from there on. Only a flag of that command is rewritten; the
// value of a flag that takes one is never rewritten (-var x=-y,
// -version -1), nor is anything after "--", a lone "-", a negative number
// or a word that names no flag (-zzz: pflag reports it). One letter after
// one dash is a shorthand already (-f, -o, -h).
func normalizeArgs(root *cobra.Command, args []string) []string {
	out := make([]string, 0, len(args))
	cmd, findCommand := root, true

	for i := 0; i < len(args); i++ {
		arg := args[i]

		switch {
		case arg == "--":
			return append(out, args[i:]...)
		case arg == "-" || !strings.HasPrefix(arg, "-"):
			if findCommand {
				if sub := subcommand(cmd, arg); sub != nil {
					cmd = sub
				} else {
					findCommand = false
				}
			}

			out = append(out, arg)

			continue
		}

		f, written := flagOf(cmd, arg)
		out = append(out, written)

		// The value of a flag that takes one is the next argument, as it
		// is: "--name -x" names the release -x.
		if f != nil && !strings.Contains(arg, "=") && f.NoOptDefVal == "" && i+1 < len(args) {
			i++
			out = append(out, args[i])
		}
	}

	return out
}

// flagOf returns the flag of cmd that arg names, and arg as pflag reads
// it: --name for -name. The flag is nil when arg names none of cmd.
func flagOf(cmd *cobra.Command, arg string) (*pflag.Flag, string) {
	// Cobra adds the flag -h, --help when it runs a command; it is needed
	// here before.
	cmd.InitDefaultHelpFlag()

	if long, ok := strings.CutPrefix(arg, "--"); ok {
		name, _, _ := strings.Cut(long, "=")

		return cmd.Flag(name), arg
	}

	short := arg[1:]
	name, _, _ := strings.Cut(short, "=")

	if len(name) == 1 {
		// A shorthand: -f values, -f=values.
		return cmd.Flags().ShorthandLookup(name), arg
	}

	if f := cmd.Flag(name); f != nil {
		return f, "-" + arg
	}

	return nil, arg
}

// subcommand returns the command of cmd that word names, nil if none does.
func subcommand(cmd *cobra.Command, word string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == word || c.HasAlias(word) {
			return c
		}
	}

	return nil
}
