// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package targztest writes the archives the tests need and archive/tar
// refuses to write.
package targztest

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strconv"
	"strings"
)

// blockSize is the size of a block of a tar.
const blockSize = 512

// SparseTar is a tar of one regular file name of size bytes that holds none
// of them: a sparse file of the format GNU 1.0, a hole from end to end. It
// is a few blocks, whatever the size.
func SparseTar(name string, size int64) []byte {
	var records strings.Builder

	for _, record := range []string{
		"GNU.sparse.major=1", "GNU.sparse.minor=0",
		"GNU.sparse.name=" + name, "GNU.sparse.realsize=" + strconv.FormatInt(size, 10),
	} {
		// The length of a record counts its own digits.
		n := len(record) + 2
		for len(strconv.Itoa(n))+len(record)+2 != n {
			n++
		}

		fmt.Fprintf(&records, "%d %s\n", n, record)
	}

	holes := padded([]byte(fmt.Sprintf("1\n%d\n0\n", size)))

	var out bytes.Buffer

	out.Write(header("PaxHeader", 'x', records.Len()))
	out.Write(padded([]byte(records.String())))
	out.Write(header("GNUSparseFile", '0', len(holes)))
	out.Write(holes)
	out.Write(make([]byte, 2*blockSize))

	return out.Bytes()
}

// SparseTarGz is SparseTar, compressed.
func SparseTarGz(name string, size int64) ([]byte, error) {
	var out bytes.Buffer

	gz := gzip.NewWriter(&out)
	if _, err := gz.Write(SparseTar(name, size)); err != nil {
		_ = gz.Close()

		return nil, err
	}

	if err := gz.Close(); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

// header is a ustar header.
func header(name string, typ byte, size int) []byte {
	const (
		modeAt, uidAt, gidAt, sizeAt, timeAt = 100, 108, 116, 124, 136
		sumAt, typeAt, magicAt               = 148, 156, 257
	)

	h := make([]byte, blockSize)
	copy(h, name)
	copy(h[modeAt:], "0000644\x00")
	copy(h[uidAt:], "0000000\x00")
	copy(h[gidAt:], "0000000\x00")
	copy(h[sizeAt:], fmt.Sprintf("%011o\x00", size))
	copy(h[timeAt:], "00000000000\x00")
	copy(h[sumAt:], "        ")
	h[typeAt] = typ
	copy(h[magicAt:], "ustar\x0000")

	var sum int
	for _, b := range h {
		sum += int(b)
	}

	copy(h[sumAt:], fmt.Sprintf("%06o\x00 ", sum))

	return h
}

// padded is b filled up to whole blocks.
func padded(b []byte) []byte {
	if n := len(b) % blockSize; n != 0 {
		return append(b, make([]byte, blockSize-n)...)
	}

	return b
}
