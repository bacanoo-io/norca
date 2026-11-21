#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
#
# Rig takes no code from source that is not open: nothing under the Business
# Source License or another license that is not open source, neither as a
# dependency nor copied (CLAUDE.md, design: rules of this work). It fails
# when a module linked into the library or the rig program has a license
# file of the Business Source License, or when a file of this repository
# carries such a license.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
pattern='Business Source License|BUSL-1\.1|SPDX-License-Identifier: BUSL'
status=0

for dir in "$root" "$root/cli"; do
  while IFS=$'\t' read -r path moddir; do
    [ -n "$moddir" ] || continue
    [ "$moddir" != "$root" ] && [ "$moddir" != "$root/cli" ] || continue
    # A module in a subdirectory of its repository (norca/norca/api)
    # falls under the license file of a directory above it.
    found=0
    for d in "$moddir" "$moddir/.." "$moddir/../.."; do
      for f in "$d"/LICENSE* "$d"/LICENCE* "$d"/COPYING*; do
        [ -f "$f" ] || continue
        found=1
        if grep -q -E "$pattern" "$f"; then
          echo "check-licenses: $path is under the Business Source License ($f)" >&2
          status=1
        fi
      done
      [ "$found" = 0 ] || break
    done
    if [ "$found" = 0 ]; then
      echo "check-licenses: $path has no license file ($moddir); check it by hand" >&2
      status=1
    fi
  done < <(cd "$dir" && go list -deps -f '{{if .Module}}{{.Module.Path}}{{"\t"}}{{if .Module.Replace}}{{.Module.Replace.Dir}}{{else}}{{.Module.Dir}}{{end}}{{end}}' ./... | sort -u)
done

# In this repository, what declares a license: an SPDX header, or a license
# file. A text that only names the license, such as this script, does not.
spdx='SPDX-License-Identifier: *(BUSL|LicenseRef-.*BUSL)'
if hits=$(git -C "$root" grep -l -I -E "$spdx" -- ':!scripts/check-licenses.sh'); then
  echo "$hits" | sed 's/^/check-licenses: a file of the repository is under the Business Source License: /' >&2
  status=1
fi

for f in "$root"/LICENSE* "$root"/LICENSES/*; do
  if [ -f "$f" ] && grep -q -E "$pattern" "$f"; then
    echo "check-licenses: $f is the Business Source License" >&2
    status=1
  fi
done

if [ "$status" = 0 ]; then
  echo "check-licenses: no dependency and no file under the Business Source License"
fi

exit "$status"
