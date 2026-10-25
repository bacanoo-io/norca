// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package threshold

// ValidateConfig checks the parameters of a threshold check, as they are
// checked when the strategy runs. It is used to validate builtin policies at
// submission.
func ValidateConfig(config map[string]string) error {
	_, err := parseConfig(config)
	return err
}
