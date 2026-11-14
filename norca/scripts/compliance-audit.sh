#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Reproducible licence audit of the Norca repository; see the "Compliance
# audit" section of the licence page of the documentation. Run it from a
# clone of the repository (any directory inside it); it checks HEAD.
#
# The tree comparison needs the upstream Nomad v1.6.5 tag. Pass a ref that
# holds it as the first argument, or let the script fetch it into
# refs/audit/nomad-v1.6.5 (the commits up to the history boundary are already
# in the clone, so only the later release/1.6.x commits are downloaded).
# The fetched objects include the 45 BUSL-1.1 files of the tag: the audit
# only reads their headers. Remove the ref afterwards with
#   git update-ref -d refs/audit/nomad-v1.6.5
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

ANCHOR=8495495badaa598c84a82affa7406bc99ef2c370 # release/1.6.x, 2023-08-09
NOMAD_TAG=a7cfff372cd52b02232a02bd3ffb5e5d35f88a0f # v1.6.5
PACK_TAG=cd4d2a41c22adba1350b8d5da63252f2dde33262  # nomad-pack v0.4.2
SWITCH=2023-08-10
# The commit that changed the licence of release/1.6.x to BSL 1.1 (upstream
# pull request 19287, merged 2024-01-02, after the v1.6.5 tag).
LICENSE_CHANGE=7a2f26e2f8af9b6220b716752a8814ca30f01878

failed=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failed=1; }

nomad_ref=${1:-}
if [ -z "$nomad_ref" ]; then
	nomad_ref=refs/audit/nomad-v1.6.5
	if ! git rev-parse -q --verify "$nomad_ref^{commit}" >/dev/null; then
		git fetch -q --no-tags https://github.com/hashicorp/nomad.git "refs/tags/v1.6.5:$nomad_ref"
	fi
fi
if [ "$(git rev-parse "$nomad_ref^{commit}")" != "$NOMAD_TAG" ]; then
	fail "$nomad_ref is not the Nomad v1.6.5 tag commit $NOMAD_TAG"
	exit 1
fi

imp=$(git log --format=%H --grep='^Import nomad v1.6.5' HEAD)
[ "$(printf '%s\n' "$imp" | wc -l)" -eq 1 ] || { fail "exactly one nomad import commit"; exit 1; }

# 1. The upstream Nomad history ends before the license change.
if [ "$(git rev-parse "$imp^2")" = "$ANCHOR" ]; then
	pass "the nomad import commit ${imp:0:12} has the upstream parent ${ANCHOR:0:12}"
else
	fail "the second parent of the nomad import commit is not ${ANCHOR:0:12}"
fi
late=$(git log --format='%aI %cI' "$ANCHOR" | awk -v s="$SWITCH" '$1 >= s || $2 >= s' | wc -l)
if [ "$late" -eq 0 ]; then
	pass "no upstream Nomad commit in the history is dated $SWITCH or later"
else
	fail "$late upstream Nomad commits are dated $SWITCH or later"
fi
if git merge-base --is-ancestor "$NOMAD_TAG" HEAD; then
	fail "the v1.6.5 tag commit is part of the history"
else
	pass "the v1.6.5 tag commit (and the release/1.6.x commits after ${ANCHOR:0:12}) is not part of the history"
fi
if git cat-file -e "$LICENSE_CHANGE^{commit}" 2>/dev/null && git merge-base --is-ancestor "$LICENSE_CHANGE" HEAD; then
	fail "the commit that changed release/1.6.x to BSL 1.1 (${LICENSE_CHANGE:0:12}) is part of the history"
else
	pass "the commit that changed release/1.6.x to BSL 1.1 (${LICENSE_CHANGE:0:12}) is not part of the history"
fi

# 2. The import tree is the v1.6.5 tree without the BUSL-1.1 files.
changes=$(git diff --name-status "$NOMAD_TAG" "$imp:nomad")
other=$(printf '%s\n' "$changes" | awk 'NF && $1 != "D"' | wc -l)
deleted=$(printf '%s\n' "$changes" | awk '$1 == "D" {print $2}')
notbusl=0
while IFS= read -r f; do
	[ -n "$f" ] || continue
	git show "$NOMAD_TAG:$f" | head -5 | grep -q 'SPDX-License-Identifier: BUSL-1.1' || notbusl=$((notbusl + 1))
done <<<"$deleted"
ndel=$(printf '%s\n' "$deleted" | grep -c . || true)
if [ "$other" -eq 0 ] && [ "$notbusl" -eq 0 ]; then
	pass "the import tree is the v1.6.5 tree without its $ndel BUSL-1.1 files"
else
	fail "the import tree differs from v1.6.5 beyond the BUSL-1.1 files ($other other changes, $notbusl non-BUSL deletions)"
fi

# 3. No upstream commit in the history adds a BUSL-1.1 header; the Norca
#    commits that mention it are documentation and tooling. uBixVault
#    came in as a squashed copy (secretstore/ubix): its upstream history is
#    not part of this repository.
up=$(git -c diff.renameLimit=0 log --format=%h -S'SPDX-License-Identifier: BUSL-1.1' "$ANCHOR" "$PACK_TAG")
if [ -z "$up" ]; then
	pass "no upstream commit in the history adds a BUSL-1.1 header"
else
	fail "upstream commits adding a BUSL-1.1 header: $up"
fi
busl_files=$( (git grep -l 'SPDX-License-Identifier: BUSL' HEAD -- ':!*.md' ':!norca/scripts/compliance-audit.sh' ':!clean-room-audit/run.sh' || true) | grep -c . || true)
if [ "$busl_files" -eq 0 ]; then
	pass "no file at HEAD carries a BUSL-1.1 header"
else
	fail "$busl_files files at HEAD carry a BUSL-1.1 header"
fi

# 4. Every modified upstream file keeps its HashiCorp copyright line and has a
#    Norca line (Modifications Copyright or SPDX-FileCopyrightText).
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
{
	git ls-tree -r "$NOMAD_TAG" | awk -F'\t' '{split($1, a, " "); p = $2; sub(/^nomad\//, "norca/", p); print "norca/" p "\t" a[3]}'
	git ls-tree -r "$PACK_TAG" | awk -F'\t' '{split($1, a, " "); print "norca/pack/" $2 "\t" a[3]}'
	# The exec2 driver came in unchanged from nomad-driver-exec2 v0.1.1 in one
	# commit; that commit's tree is the upstream version of its files.
	exec2=$(git log --format=%H --grep='^drivers/exec2: import nomad-driver-exec2' HEAD)
	[ -z "$exec2" ] || git ls-tree -r "$exec2" -- norca/drivers/exec2 | awk -F'\t' '{split($1, a, " "); print $2 "\t" a[3]}'
} | LC_ALL=C sort >"$t/base"
git ls-tree -r HEAD -- norca | awk -F'\t' '{split($1, a, " "); print $2 "\t" a[3]}' | LC_ALL=C sort >"$t/head"
LC_ALL=C join -t "$(printf '\t')" -a 1 -e - -o 0,1.2,2.2 "$t/head" "$t/base" |
	awk -F'\t' '$2 != $3 {print $1}' >"$t/changed"
git cat-file --batch='@@NORCA-AUDIT@@' <<<"$(sed 's/^/HEAD:/' "$t/changed")" |
	awk -v list="$t/changed" '
		BEGIN { while ((getline f < list) > 0) files[++n] = f }
		/^@@NORCA-AUDIT@@$/ { i++; lines = 0; next }
		lines < 12 { h[i] = h[i] "\n" $0; lines++ }
		END {
			for (j = 1; j <= n; j++)
				if (h[j] ~ /Copyright (\(c\) )?(HashiCorp|IBM Corp)/ && h[j] !~ /Norca/) print files[j]
		}' >"$t/missing"
nchanged=$(wc -l <"$t/changed")
nmissing=$(wc -l <"$t/missing")
if [ "$nmissing" -eq 0 ]; then
	pass "all $nchanged modified upstream files carry a Norca copyright line"
else
	fail "$nmissing of $nchanged modified upstream files have no Norca copyright line:"
	sed 's/^/      /' "$t/missing"
fi

# 5. No upstream Nomad module comes from the module proxy.
if command -v go >/dev/null; then
	for m in norca norca/api norca/controller norca/tools; do
		mods=$(cd "$m" && go list -m all)
		bad=$(printf '%s\n' "$mods" | awk '
			$1 == "github.com/hashicorp/nomad" { print; next }
			index($1, "github.com/hashicorp/nomad/") == 1 || $1 == "github.com/bacanoo-io/norca/norca" || index($1, "github.com/bacanoo-io/norca/norca/") == 1 {
				if (NF > 1 && !($3 == "=>" && $4 ~ /^\.\.?\//)) print
			}')
		if [ -z "$bad" ]; then
			pass "$m: no upstream Nomad module; Norca modules and the upstream API stand-in are local"
		else
			fail "$m: $bad"
		fi
	done
else
	printf 'SKIP  Go module check (go is not installed)\n'
fi

# 6. The result of the clean room audit is in the repository (CLEAN_ROOM.md):
#    file names and scores only, of a commit of this history.
missing=0
for f in CLEAN_ROOM.md clean-room-result/pairs.csv clean-room-result/summary.md; do
	git cat-file -e "HEAD:$f" 2>/dev/null || { fail "$f is not in the repository"; missing=1; }
done
if [ "$missing" -eq 0 ]; then
	columns=$(git show HEAD:clean-room-result/pairs.csv | sed -n 1p | tr -d '\r')
	want=set,language,norca_file,upstream_file,similarity,longest_fragment,total_overlap,same_path,norca_kind,mpl_baseline,char_similarity,candidate
	if [ "$columns" = "$want" ]; then
		pass "clean-room-result/pairs.csv has file names and scores only"
	else
		fail "clean-room-result/pairs.csv has other columns: $columns"
	fi
	audited=$(git show HEAD:clean-room-result/summary.md | sed -n 's/^| norca_commit | `\([0-9a-f]*\)` |.*/\1/p')
	if [ -n "$audited" ] && git merge-base --is-ancestor "$audited" HEAD 2>/dev/null; then
		pass "the clean room result is of ${audited:0:12}, $(git rev-list --count "$audited..HEAD") commits before HEAD"
	else
		fail "the clean room result names no commit of this history: '$audited'"
	fi
fi

exit "$failed"
