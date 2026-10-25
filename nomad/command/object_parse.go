// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/helper/resourcespec"
)

// readObjectInput reads a specification file, or stdin for "-". It returns
// the content and a file name whose extension selects the format.
func readObjectInput(path string) ([]byte, string, error) {
	if path == "-" {
		content, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, "", fmt.Errorf("failed to read stdin: %w", err)
		}
		return content, "stdin.hcl", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read file %q: %w", path, err)
	}
	return content, path, nil
}

// isJSONInput reports whether the input is in JSON, either because -json
// was given or because the file has the .json extension.
func isJSONInput(path string, jsonFlag bool) bool {
	return jsonFlag || strings.HasSuffix(strings.ToLower(path), ".json")
}

// parseObjectKind parses an object kind in HCL or, for JSON input, in the
// API JSON form.
func parseObjectKind(path string, content []byte, jsonInput bool) (*api.ObjectKind, error) {
	return resourcespec.ParseObjectKind(path, content, jsonInput)
}

// parseCustomObject parses an object in HCL or, for JSON input, in the API
// JSON form.
func parseCustomObject(path string, content []byte, jsonInput bool) (*api.CustomObject, error) {
	return resourcespec.ParseCustomObject(path, content, jsonInput)
}
