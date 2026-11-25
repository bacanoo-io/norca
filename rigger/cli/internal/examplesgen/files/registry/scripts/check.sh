#!/usr/bin/env sh
# The checks of the registry, without a cluster: what a CI runs on every
# change. Run from the root of the registry: scripts/check.sh
# RIGGER names another rigger program (default: rigger on the PATH).
set -eu

rigger=${RIGGER:-rigger}

# The files are formatted as rigger fmt writes them.
"$rigger" fmt --check modules stacks

# Every module: its files, its icon, its tests parsed, its defaults and
# every file of examples/ rendered, the jobs decoded as Norca decodes them;
# then its tests. A library module (lib/) is checked by the modules that
# call it.
for dir in modules/*/; do
  name=$(basename "$dir")
  [ "$name" = lib ] && continue
  "$rigger" module validate "$dir"
  if [ -d "$dir/tests" ]; then
    "$rigger" module test "$dir"
  fi
done

# Every stack with its lock file, with each of its example value files.
for dir in stacks/*/; do
  if [ ! -f "$dir/rigger.lock.hcl" ]; then
    echo "check: $dir has no rigger.lock.hcl; run rigger stack sync $dir" >&2
    exit 1
  fi
  for values in "$dir"examples/*.vars.hcl; do
    [ -f "$values" ] || continue
    "$rigger" stack validate "$dir" -f "$values"
  done
done

echo "check: all modules and stacks are valid"
