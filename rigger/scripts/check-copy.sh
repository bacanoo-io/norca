#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
#
# Compares the copy of norca's jobspec2 (cli/internal/jobspec2, SOURCE.md)
# and of a file of its resourcespec (cli/internal/resourcespec) with the
# norca module of this repository (../norca). It fails when a copied
# file differs, or when norca's jobspec2 has a non-test file the copy lacks.
# NORCA_DIR overrides the place of the norca module.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
copy="$root/cli/internal/jobspec2"
norca="${NORCA_DIR:-$root/../norca}/jobspec2"

if [ ! -d "$norca" ]; then
  echo "check-copy: no norca checkout at $norca (set NORCA_DIR)" >&2
  exit 1
fi

status=0

for f in "$norca"/*.go; do
  name="$(basename "$f")"
  case "$name" in *_test.go) continue ;; esac

  if [ ! -f "$copy/$name" ]; then
    echo "check-copy: $name is in norca's jobspec2, not in the copy" >&2
    status=1
  elif ! cmp -s "$f" "$copy/$name"; then
    echo "check-copy: $name differs from norca's" >&2
    status=1
  fi
done

for f in "$copy"/*.go; do
  name="$(basename "$f")"
  case "$name" in rig_*) continue ;; esac

  if [ ! -f "$norca/$name" ]; then
    echo "check-copy: $name is in the copy, not in norca's jobspec2" >&2
    status=1
  fi
done

# The copied files of norca's resourcespec (cli/internal/resourcespec,
# SOURCE.md).
specs="${NORCA_DIR:-$root/../norca}/helper/resourcespec"
for name in object.go; do
  if ! cmp -s "$specs/$name" "$root/cli/internal/resourcespec/$name"; then
    echo "check-copy: resourcespec/$name differs from norca's" >&2
    status=1
  fi
done

if [ "$status" -eq 0 ]; then
  echo "check-copy: the copies of jobspec2 and resourcespec match norca"
fi

exit "$status"
