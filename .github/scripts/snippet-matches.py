#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
"""Reports every match of the SCANOSS scan and stops on a match with BSL code.

usage: snippet-matches.py <scanoss result file> <scanoss settings file>

A match with a component that scanoss.json declares (the works Norca is
built on, see NOTICE) is listed and nothing more. A match with any other
component is a warning: the code is either Norca's own and the match is a
coincidence, or it came from that component and needs its attribution. A
match whose license is the Business Source License fails the job: such code
must not be in the repository, whatever the similarity audit says.
"""
import json
import os
import sys

BLOCKED = ('BUSL', 'BSL-1.1', 'Business Source')


def declared_purls(settings_path):
    """The purls scanoss.json declares, without versions."""
    try:
        with open(settings_path, encoding='utf-8') as f:
            settings = json.load(f)
    except (OSError, ValueError):
        return set()
    return {base(e['purl']) for e in settings.get('bom', {}).get('include', []) if e.get('purl')}


def base(purl):
    """The purl without its version and qualifiers."""
    return purl.split('@')[0].split('?')[0]


def main():
    result_path, settings_path = sys.argv[1], sys.argv[2]
    try:
        with open(result_path, encoding='utf-8') as f:
            results = json.load(f)
    except (OSError, ValueError) as err:
        print(f'::warning::no SCANOSS result to read ({err}); nothing was checked')
        return 0

    declared = declared_purls(settings_path)
    rows, blocked, undeclared = [], 0, 0
    for path in sorted(results):
        for match in results[path] or []:
            kind = match.get('id', 'none')
            if kind == 'none':
                continue
            purls = match.get('purl') or []
            licenses = sorted({lic.get('name', '') for lic in match.get('licenses') or [] if lic.get('name')})
            name = purls[0] if purls else match.get('component', '?')
            version = match.get('version', '')
            matched = match.get('matched', '?')
            license_text = ', '.join(licenses) or 'license unknown'
            is_blocked = any(b.lower() in lic.lower() for lic in licenses for b in BLOCKED)
            is_declared = any(base(p) in declared for p in purls)
            if is_blocked:
                blocked += 1
                state = 'BSL: not allowed'
                print(f'::error file={path}::{kind} match with {name} {version} under {license_text}: '
                      'code under the Business Source License must not be in the repository')
            elif is_declared:
                state = 'declared'
            else:
                undeclared += 1
                state = 'not declared'
                print(f'::warning file={path}::{kind} match with {name} {version} ({license_text}, {matched}): '
                      'if the code comes from there, it needs its attribution and an entry in scanoss.json')
            rows.append((path, kind, matched, name, version, license_text, state))

    lines = ['## Snippet scan: every match', '',
             f'{len(rows)} matches: {blocked} under BSL, {undeclared} with a component that is not declared.', '']
    if rows:
        lines += ['| File | Kind | Matched | Component | Version | License | State |',
                  '| --- | --- | --- | --- | --- | --- | --- |']
        lines += ['| ' + ' | '.join(str(c).replace('|', '/') for c in row) + ' |' for row in rows]
    text = '\n'.join(lines) + '\n'
    summary = os.environ.get('GITHUB_STEP_SUMMARY')
    if summary:
        with open(summary, 'a', encoding='utf-8') as f:
            f.write(text)
    else:
        print(text)
    return 1 if blocked else 0


if __name__ == '__main__':
    sys.exit(main())
