/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

// @ts-check

/**
 * Escapes a string so it can be placed between double quotes in HCL2.
 * Besides the usual backslash escapes, the template introducers `${` and
 * `%{` are doubled so the value is taken literally instead of being
 * interpreted as an interpolation or directive.
 *
 * @param {string} value
 * @returns {string}
 */
export function escapeHclString(value) {
  return value
    .replace(/\\/g, '\\\\')
    .replace(/"/g, '\\"')
    .replace(/\n/g, '\\n')
    .replace(/\r/g, '\\r')
    .replace(/\t/g, '\\t')
    .replace(/\$\{/g, () => '$${') // function form: "$$" in a pattern string means "$"
    .replace(/%\{/g, () => '%%{');
}

/**
 * Turns a flat object of variable values (such as the `VariableFlags` map of
 * a job submission) into HCL variable assignments, one `key="value"` line
 * per entry. The result ends with a newline so further variable text can be
 * concatenated directly.
 *
 * Strings are quoted and escaped; numbers and booleans are written as bare
 * literals. Empty or missing input yields an empty string.
 *
 * @param {Record<string, unknown> | null | undefined} obj
 * @returns {string}
 */
export default function jsonToHcl(obj) {
  if (!obj || typeof obj !== 'object') {
    return '';
  }

  const lines = Object.keys(obj).map((key) => {
    const value = obj[key];
    const hclValue =
      typeof value === 'string'
        ? `"${escapeHclString(value)}"`
        : typeof value === 'number' || typeof value === 'boolean'
        ? String(value)
        : `"${escapeHclString(JSON.stringify(value) ?? '')}"`;
    return `${key}=${hclValue}\n`;
  });

  return lines.join('\n');
}
