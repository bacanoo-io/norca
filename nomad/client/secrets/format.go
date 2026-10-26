// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"github.com/hashicorp/nomad/nomad/structs"
)

// Render returns the contents of a secret file in the file's format:
//
//   - env: one KEY="value" line per item, sorted, readable by envparse and
//     shells (", \, $ and ` escaped, newlines as \n)
//   - json: a JSON object of the items
//   - raw:<item>: the value of one item
//   - template: the Go template of the file, with the items as its data
//
// Errors never contain values.
func Render(file *structs.TaskSecretFile, items map[string]string) ([]byte, error) {
	switch {
	case file.Format == "" || file.Format == structs.SecretFileFormatEnv:
		var buf bytes.Buffer
		for _, k := range sortedKeys(items) {
			fmt.Fprintf(&buf, "%s=\"%s\"\n", k, escapeEnvValue(items[k]))
		}
		return buf.Bytes(), nil
	case file.Format == structs.SecretFileFormatJSON:
		b, err := json.Marshal(items)
		if err != nil {
			return nil, fmt.Errorf("failed to encode the secret as JSON")
		}
		return append(b, '\n'), nil
	case file.Format == structs.SecretFileFormatTemplate:
		tmpl, err := template.New("secret").Option("missingkey=error").Parse(file.Template)
		if err != nil {
			return nil, fmt.Errorf("invalid secret file template: %v", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, items); err != nil {
			// The error of a template can quote data; report only that it
			// failed.
			return nil, fmt.Errorf("failed to render the secret file template (missing item?)")
		}
		return buf.Bytes(), nil
	case file.RawItem() != "":
		v, ok := items[file.RawItem()]
		if !ok {
			return nil, fmt.Errorf("the secret has no item %q", file.RawItem())
		}
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("invalid secret file format %q", file.Format)
	}
}

// EnvValues returns the environment variables of a secret block: variable
// name to the value of its item.
func EnvValues(env map[string]string, items map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for name, item := range env {
		v, ok := items[item]
		if !ok {
			return nil, fmt.Errorf("environment variable %s: the secret has no item %q", name, item)
		}
		out[name] = v
	}
	return out, nil
}

func escapeEnvValue(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`", "\n", `\n`).Replace(v)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Fingerprint is a hash of the items for change detection, also for
// providers without versions. It is only kept in memory and never shown: an
// unkeyed hash would allow guessing weak values offline.
func Fingerprint(items map[string]string) string {
	h := sha256.New()
	for _, k := range sortedKeys(items) {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(items[k]), items[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
