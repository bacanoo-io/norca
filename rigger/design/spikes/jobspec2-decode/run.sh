#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
#
# Runs the spike of question 4 against the norca checkout next to this
# repository, without changing it: go test -overlay adds the files of this
# directory to the package jobspec2 for this run only.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
norca="${NORCA_DIR:-$here/../../../../norca/norca}"
pkg="$(cd "$norca/jobspec2" && pwd)"
overlay="$(mktemp)"
trap 'rm -f "$overlay"' EXIT

{
  printf '{"Replace":{'
  sep=""
  for f in "$here"/zz_*.go; do
    printf '%s"%s/%s":"%s"' "$sep" "$pkg" "$(basename "$f")" "$f"
    sep=","
  done
  printf '}}\n'
} > "$overlay"

cd "$norca"
GOWORK=off go test -overlay "$overlay" -run TestRigSpike -count=1 -v ./jobspec2
