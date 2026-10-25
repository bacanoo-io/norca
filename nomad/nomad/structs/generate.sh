#!/usr/bin/env bash
# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0
# SPDX-FileCopyrightText: 2026 Norca contributors

set -e

# Go 1.25+ rejects base64 alphabets with duplicate symbols, and the codecgen
# generator in go-msgpack v1.1.5 uses one ("...0123456789__") to build type
# names. That code is only compiled into the generator (codecgen.exec tag),
# never into Nomad, so generate with a patched copy of the module that encodes
# with the URL alphabet and maps '-' to '_', which yields identical names.
MSGPACK_MOD="github.com/hashicorp/go-msgpack"
MSGPACK_VERSION="$(go list -m -f '{{.Version}}' "${MSGPACK_MOD}")"
MSGPACK_DIR="$(go list -m -f '{{.Dir}}' "${MSGPACK_MOD}")"
MODULE_DIR="$(go list -m -f '{{.Dir}}')"

PATCHED="$(mktemp -d)"
# codecgen removes its output file before generating; keep a copy so a failed
# run leaves the committed file intact instead of an empty one
cp structs.generated.go "${PATCHED}/structs.generated.go.orig"
cleanup() {
	status=$?
	if [ "${status}" -ne 0 ]; then
		cp "${PATCHED}/structs.generated.go.orig" structs.generated.go
		echo "codecgen failed; structs.generated.go restored" >&2
	fi
	chmod -R u+w "${PATCHED}"
	rm -rf "${PATCHED}"
}
trap cleanup EXIT
cp -R "${MSGPACK_DIR}/." "${PATCHED}/go-msgpack"
chmod -R u+w "${PATCHED}/go-msgpack"
sed -i \
    -e 's|base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789__")|base64.URLEncoding|' \
    -e 's|^\(\t*\)genBase64enc.Encode(bufx, \[\]byte(tstr))$|&\n\1bufx = bytes.ReplaceAll(bufx, []byte("-"), []byte("_"))|' \
    "${PATCHED}/go-msgpack/codec/gen.go"
grep -q 'base64.URLEncoding' "${PATCHED}/go-msgpack/codec/gen.go"
grep -q 'bytes.ReplaceAll(bufx' "${PATCHED}/go-msgpack/codec/gen.go"

cat > "${PATCHED}/go.work" <<EOF
go $(go list -m -f '{{.GoVersion}}')

use (
	${MODULE_DIR}
	${MODULE_DIR}/api
)

replace ${MSGPACK_MOD} ${MSGPACK_VERSION} => ${PATCHED}/go-msgpack
EOF
export GOWORK="${PATCHED}/go.work"

FILES="$(ls ./*.go | grep -v -e _test.go -e .generated.go | tr '\n' ' ')"
codecgen \
    -c github.com/hashicorp/go-msgpack/codec \
    -st codec \
    -d 100 \
    -t codegen_generated \
    -o structs.generated.go \
    -nr="(^ACLCache$)|(^IdentityClaims$)" \
    ${FILES}
