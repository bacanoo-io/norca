#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0
#
# Runs inside the throwaway container of audit.ps1 (see README.md). Clones
# Norca and fetches the two compared Nomad tags, extracts the compared files
# into /work, runs Dolos and writes only file pairs and scores to /out. /work
# (the only place with HashiCorp code) disappears with the container.
#
# Environment: NORCA_REF, BSL_TAG (a tag or "latest"), SETS (busl, bugfix,
# full; default busl,bugfix),
# THRESHOLD, DOLOS_VERSION; optional NORCA_URL, the repository to clone Norca
# from (default: GitHub; audit.ps1 mounts the checkout it is part of
# read-only at /norca-src and passes that path); optional CA_CERT, a PEM file
# of extra certificate authorities for npm and git (a proxy or an antivirus
# that intercepts TLS).
set -euo pipefail

# The whole script is one function, so bash reads it completely before it
# runs: editing this file while an audit runs does not affect that audit.
main() {

	NORCA_URL=${NORCA_URL:-https://github.com/bacanoo-io/norca.git}
	NOMAD_URL=https://github.com/hashicorp/nomad.git
	BASE_TAG=v1.6.5
	W=/work
	mkdir -p "$W" /out

	log() { printf '==> %s\n' "$*" >&2; }

	# The clones are thrown away with the container; automatic gc would only
	# print noise. The global config lives in the container too.
	git config --global gc.auto 0
	# A mounted local checkout belongs to another user than the container.
	git config --global --add safe.directory '*'
	git config --global maintenance.auto false

	# Extra certificate authorities: Node adds them to its own, git gets the
	# system bundle with them appended.
	if [ -n "${CA_CERT:-}" ]; then
		[ -r "$CA_CERT" ] || { log "CA_CERT $CA_CERT is not readable"; exit 1; }
		export NODE_EXTRA_CA_CERTS=$CA_CERT
		cat /etc/ssl/certs/ca-certificates.crt "$CA_CERT" >/tmp/ca-bundle.crt
		git config --global http.sslCAInfo /tmp/ca-bundle.crt
		log "using the extra certificate authorities of $CA_CERT"
	fi

	# tls_hint explains a failed download when the log shows a certificate
	# that does not verify.
	tls_hint() {
		if grep -qiE 'certificate|UNABLE_TO_VERIFY|SELF_SIGNED|unable to get local issuer' "$1" 2>/dev/null; then
			log "a certificate did not verify: a proxy or an antivirus may intercept TLS;" \
				"give its root certificate with -CACert (audit.ps1) or CA_CERT, or turn the interception off for the run"
		fi
	}

	# retry runs a network command up to four times: GitHub occasionally drops
	# a TLS connection ("gnutls_handshake() failed").
	retry() {
		local i
		for i in 1 2 3 4; do
			"$@" && return 0
			log "attempt $i failed: $*"
			sleep $((i * 10))
		done
		return 1
	}

	log "installing Dolos $DOLOS_VERSION"
	npm_install() { npm install -g --no-fund --no-audit --loglevel=error "@dodona/dolos@$DOLOS_VERSION" >/tmp/npm-install.log 2>&1; }
	if ! retry npm_install; then
		log "installing Dolos failed; the end of the npm log:"
		tail -n 15 /tmp/npm-install.log >&2
		tls_hint /tmp/npm-install.log
		exit 1
	fi

	# Norca: a full clone (the clean room commits need their parents' trees).
	# Nomad: only the two tag commits, each fetched once with its whole tree,
	# so no blob is fetched later on demand.
	log "cloning Norca ($NORCA_REF)"
	if ! retry git clone -q --no-checkout "$NORCA_URL" "$W/norca" 2>/tmp/git-clone.log; then
		tail -n 15 /tmp/git-clone.log >&2
		tls_hint /tmp/git-clone.log
		exit 1
	fi
	# A clone has a local branch only for the checked-out branch of its source;
	# other branches are origin/<name>.
	norca_rev=$(git -C "$W/norca" rev-parse -q --verify "$NORCA_REF^{commit}" ||
		git -C "$W/norca" rev-parse -q --verify "origin/$NORCA_REF^{commit}") ||
		{ log "unknown Norca revision $NORCA_REF"; exit 1; }
	if [ "$BSL_TAG" = latest ]; then
		# The highest release tag of any major version (v2.1.1 is after
		# v1.11.3); pre-releases (-beta, -rc) do not count.
		BSL_TAG=$(retry git ls-remote --tags --refs "$NOMAD_URL" 'v*' 2>/tmp/git-tags.log | sed 's|.*refs/tags/||' |
			grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1) || true
		if [ -z "$BSL_TAG" ]; then
			log "could not list the Nomad tags of $NOMAD_URL:"
			tail -n 15 /tmp/git-tags.log >&2
			tls_hint /tmp/git-tags.log
			exit 1
		fi
	fi
	log "fetching Nomad $BASE_TAG and $BSL_TAG"
	git init -q "$W/nomad"
	retry git -C "$W/nomad" fetch -q --depth=1 --no-tags "$NOMAD_URL" \
		"refs/tags/$BASE_TAG:refs/tags/$BASE_TAG" "refs/tags/$BSL_TAG:refs/tags/$BSL_TAG"
	git -C "$W/nomad" rev-parse -q --verify "$BSL_TAG^{commit}" >/dev/null || { log "no tag $BSL_TAG"; exit 1; }

	# put <repo> <rev> <path> <dest>: writes a file of a revision to dest.
	put() {
		mkdir -p "$(dirname "$4")"
		git -C "$W/$1" show "$2:$3" >"$4"
	}

	lang_of() {
		case "$1" in
		*.go) echo go ;;
		*.js) echo javascript ;;
		*.ts) echo typescript ;;
		*) echo char ;;
		esac
	}

	# The Norca tree changed its layout over time. layout <rev> prints:
	#   A  the Nomad code is in nomad/ (nomad/nomad/ is the server package)
	#   B  it is in norca/ (norca/nomad/ is the server package)
	#   C  it is in norca/ and the server package is norca/norca/
	layout() {
		if git -C "$W/norca" cat-file -e "$1:norca/norca" 2>/dev/null; then
			echo C
		elif git -C "$W/norca" cat-file -e "$1:norca/go.mod" 2>/dev/null; then
			echo B
		else
			echo A
		fi
	}

	# top_of <layout>: the directory of the Nomad code, with a trailing slash.
	top_of() {
		if [ "$1" = A ]; then echo nomad/; else echo norca/; fi
	}

	# np <layout> <upstream path>: the path in Norca of a v1.6.5 file or directory.
	np() {
		local p=$2
		case $1 in
		A) echo "nomad/$p" ;;
		B) echo "norca/$p" ;;
		C) case $p in
			nomad | nomad/*) echo "norca/norca${p#nomad}" ;;
			*) echo "norca/$p" ;;
			esac ;;
		esac
	}

	# up_of <layout> <Norca path>: the inverse of np, the path in v1.6.5.
	up_of() {
		local q
		case $1 in
		A) echo "${2#nomad/}" ;;
		B) echo "${2#norca/}" ;;
		C) q=${2#norca/}
			case $q in
			norca | norca/*) echo "nomad${q#norca}" ;;
			*) echo "$q" ;;
			esac ;;
		esac
	}

	# norm: a source without the Go module path and the Norca header lines, so
	# that a restored file compares equal to its MPL original line by line.
	norm() {
		sed -E 's#github\.com/(hashicorp/nomad|bacanoo-io/norca/(nomad|norca))/#MOD/#g; s#MOD/(nomad|norca)/#MOD/server/#g' |
			grep -v -E '^// (SPDX-FileCopyrightText|Norca:)' || true
	}

	# restored <anchor> <upstream path> <Norca path>: true if the MPL anchor
	# tree has a file at the upstream path and the Norca file differs from it,
	# after norm, in at most 10% of the lines of the longer one. Such a file is
	# HashiCorp's pre-switch MPL code that v1.6.5 replaced with a BUSL file; it
	# is resembled by that BUSL file because the BUSL file grew out of it. The
	# file and its line counts go to restored.tsv for the report.
	restored() {
		local a n d max
		git -C "$W/norca" cat-file -e "$1:$2" 2>/dev/null || return 1
		a=$(git -C "$W/norca" show "$1:$2" | norm | wc -l)
		n=$(git -C "$W/norca" show "$norca_rev:$3" | norm | wc -l)
		d=$(diff <(git -C "$W/norca" show "$1:$2" | norm) <(git -C "$W/norca" show "$norca_rev:$3" | norm) | grep -c '^[<>]' || true)
		max=$((a > n ? a : n))
		[ "$max" -gt 0 ] && [ $((d * 10)) -le "$max" ] || return 1
		printf '%s\t%s\t%s\n' "$2" "$max" "$d" >>"$W/result/busl-restored.tsv"
	}

	# dolos_set <set> [dolos options]: runs Dolos per language over
	# $W/<set>/{norca,upstream,...} and keeps the pairs.csv files.
	dolos_set() {
		local set=$1 lang files
		shift
		for lang in go javascript typescript char; do
			mapfile -t files < <(find "$W/$set" -type f -name "*.$lang.src" | sort)
			[ "${#files[@]}" -ge 2 ] || continue
			log "$set: Dolos ($lang) over ${#files[@]} files"
			dolos run -f csv -o "$W/result/$set-$lang" -l "$lang" "$@" "${files[@]}" >/dev/null
			cp "$W/result/$set-$lang/pairs.csv" "$W/result/$set-$lang.pairs.csv"
		done
	}

	mkdir -p "$W/result"

	# Set busl: the 45 files of v1.6.5 with a BUSL-1.1 header against the Norca
	# files of the same extension in the same directories (where the clean room
	# replacements live).
	if [[ ",$SETS," == *,busl,* ]]; then
		# The import commit lists the files it left out, one per line, indented.
		imp=$(git -C "$W/norca" log --format=%H --grep='^Import nomad v1.6.5' "$norca_rev")
		mapfile -t busl < <(git -C "$W/norca" log -1 --format=%B "$imp" | sed -n 's|^    nomad/||p')
		log "busl: ${#busl[@]} BUSL-1.1 files"
		L=$(layout "$norca_rev")
		# The second parent of the import is the anchor: the last release/1.6.x
		# commit before the license change, whose tree is all MPL.
		anchor=$(git -C "$W/norca" rev-parse "$imp^2")
		for f in "${busl[@]}"; do
			# REUSE-IgnoreStart (the next line searches HashiCorp's header, it is not this file's license)
			git -C "$W/nomad" show "$BASE_TAG:$f" | head -5 | grep -q 'SPDX-License-Identifier: BUSL-1.1' ||
				{ log "$f has no BUSL-1.1 header in $BASE_TAG"; exit 1; }
			# REUSE-IgnoreEnd
			l=$(lang_of "$f")
			put nomad "$BASE_TAG" "$f" "$W/busl/upstream/$f.$l.src"
			dir=$(dirname "$f")
			ext=${f##*.}
			while IFS= read -r g; do
				# u is the v1.6.5 path of the Norca file g, whatever the layout.
				u=$(up_of "$L" "$g")
				put norca "$norca_rev" "$g" "$W/busl/norca/$u.$l.src"
				# The kind of the Norca file: "mpl" if it comes from the import
				# (HashiCorp's own MPL file, possibly with Norca changes), "new"
				# if Norca added it. The MPL files give the baseline of a BUSL
				# file: how similar HashiCorp's own MPL code is to it. The
				# import commit has the layout A. "restored" is a file that
				# v1.6.5 no longer has but the MPL anchor tree has, and that
				# Norca brought back nearly unchanged (see restored).
				if git -C "$W/norca" cat-file -e "$imp:$(np A "$u")" 2>/dev/null; then
					k=mpl
				elif restored "$anchor" "$u" "$g"; then
					k=restored
				else
					k=new
				fi
				printf '%s\t%s\n' "$u" "$k" >>"$W/result/busl-kinds.tsv"
				# Same-path pairs are compared once more character by
				# character, which also counts names and literals.
				if [ "$u" = "$f" ]; then
					mkdir -p "$(dirname "$W/busl-char/norca/$f")" "$(dirname "$W/busl-char/upstream/$f")"
					cp "$W/busl/norca/$f.$l.src" "$W/busl-char/norca/$f.char.src"
					cp "$W/busl/upstream/$f.$l.src" "$W/busl-char/upstream/$f.char.src"
				fi
			done < <(git -C "$W/norca" ls-tree -r --name-only "$norca_rev" -- "$(np "$L" "$dir")/" | grep -E "\.$ext\$" || true)
		done
		dolos_set busl
		[ -d "$W/busl-char" ] && dolos_set busl-char
	fi

	# Set bugfix: the lines each clean room commit added to a file against the
	# lines HashiCorp added to the same file between v1.6.5 and $BSL_TAG.
	if [[ ",$SETS," == *,bugfix,* ]]; then
		mapfile -t commits < <({
			git -C "$W/norca" log --no-merges --format=%H -i -E --grep='(clean|white) room' "$norca_rev"
			sed -e 's/#.*//' /audit/commits.txt | awk 'NF {print $1}' |
				while read -r h; do git -C "$W/norca" rev-parse -q --verify "$h^{commit}" || log "unknown commit $h"; done
		} | sort -u)
		log "bugfix: ${#commits[@]} clean room commits, BSL tag $BSL_TAG"
		for c in "${commits[@]}"; do
			# The layout of the commit decides where the Nomad code is.
			Lc=$(layout "$c")
			top=$(top_of "$Lc")
			while IFS= read -r f; do
				[ -n "$f" ] || continue
				up=$(up_of "$Lc" "$f")
				git -C "$W/nomad" cat-file -e "$BSL_TAG:$up" 2>/dev/null || continue
				l=$(lang_of "$f")
				n="$W/bugfix/norca/${c:0:12}/$up.$l.src"
				mkdir -p "$(dirname "$n")"
				git -C "$W/norca" diff -U0 "$c^" "$c" -- "$f" | sed -n 's/^+\([^+]\)/\1/p; s/^+$//p' >"$n"
				[ -s "$n" ] || { rm -f "$n"; continue; }
				u="$W/bugfix/upstream/$up.$l.src"
				if [ ! -e "$u" ]; then
					mkdir -p "$(dirname "$u")"
					git -C "$W/nomad" diff -U0 "$BASE_TAG" "$BSL_TAG" -- "$up" | sed -n 's/^+\([^+]\)/\1/p; s/^+$//p' >"$u"
					[ -s "$u" ] || rm -f "$u"
				fi
			done < <(git -C "$W/norca" diff --name-only "$c^" "$c" -- "$top*.go" "$top*.js" "$top*.ts" "$top*.hbs")
		done
		# Fragments of added lines do not parse as Go or JavaScript: compare them
		# character by character.
		find "$W/bugfix" -type f -name '*.src' | while IFS= read -r p; do
			case "$p" in *.char.src) ;; *) mv "$p" "${p%.*.src}.char.src" ;; esac
		done
		dolos_set bugfix
	fi

	# Set full: every Norca file of the Nomad code that Norca wrote or changed
	# (new since the import, or different from its v1.6.5 file once the module
	# path, the Norca header lines and the server package name are set aside)
	# against every file of $BSL_TAG with the same language, whatever its path.
	# The v1.6.5 file of a changed Norca file comes along as its base: the
	# similarity of HashiCorp's own MPL code to a BSL file, which a changed
	# Norca file may reach without copying anything. Generated files are left
	# out on both sides. Dolos reports only pairs of at least half the
	# threshold.
	if [[ ",$SETS," == *,full,* ]]; then
		L=$(layout "$norca_rev")
		top=$(top_of "$L")
		skip='(\.generated\.go|\.pb\.go|bindata_assetfs\.go)$'
		fnorm() { norm | grep -v -E '^// Modifications Copyright' | sed -E 's/^package norca$/package nomad/' || true; }
		n_new=0 n_mod=0 n_same=0
		while IFS= read -r f; do
			u=$(up_of "$L" "$f")
			l=$(lang_of "$f")
			if git -C "$W/nomad" cat-file -e "$BASE_TAG:$u" 2>/dev/null; then
				if cmp -s <(git -C "$W/nomad" show "$BASE_TAG:$u" | fnorm) <(git -C "$W/norca" show "$norca_rev:$f" | fnorm); then
					n_same=$((n_same + 1))
					continue
				fi
				k=modified
				n_mod=$((n_mod + 1))
				put nomad "$BASE_TAG" "$u" "$W/full/base/$u.$l.src"
			else
				k=new
				n_new=$((n_new + 1))
			fi
			put norca "$norca_rev" "$f" "$W/full/norca/$u.$l.src"
			printf '%s\t%s\n' "$u" "$k" >>"$W/result/full-kinds.tsv"
		done < <(git -C "$W/norca" ls-tree -r --name-only "$norca_rev" -- "$top" | grep -E '\.(go|js|ts)$' | grep -v -E "$skip" || true)
		n_up=0
		while IFS= read -r u; do
			put nomad "$BSL_TAG" "$u" "$W/full/upstream/$u.$(lang_of "$u").src"
			n_up=$((n_up + 1))
		done < <(git -C "$W/nomad" ls-tree -r --name-only "$BSL_TAG" | grep -E '\.(go|js|ts)$' | grep -v -E "$skip" || true)
		log "full: $n_new new and $n_mod changed Norca files ($n_same unchanged MPL files left out), $n_up files of $BSL_TAG"
		NODE_OPTIONS=--max-old-space-size=${FULL_HEAP_MB:-16384} dolos_set full \
			-S "$(awk -v t="$THRESHOLD" 'BEGIN { print t / 2 }')"
	fi

	node /audit/report.mjs "$W/result" /out "$THRESHOLD" <<-EOF
	norca_ref=$NORCA_REF
	norca_commit=$norca_rev
	base_tag=$BASE_TAG
	bsl_tag=$BSL_TAG
	bsl_commit=$(git -C "$W/nomad" rev-parse "$BSL_TAG^{commit}")
	dolos=$DOLOS_VERSION
	sets=$SETS
	threshold=$THRESHOLD
	date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	EOF
}

main "$@"
