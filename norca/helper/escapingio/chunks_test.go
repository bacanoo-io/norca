// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package escapingio

import (
	"bytes"
	"io"
	"testing"
)

// chunkReader returns the chunks one per Read.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(buf []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(buf, r.chunks[0])
	if n < len(r.chunks[0]) {
		r.chunks[0] = r.chunks[0][n:]
	} else {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

// TestEscapingReader_ChunkEndsAfterEscapedNewline checks an input whose read
// ends right after `~\n`: the next read starts a line, so its `~.` is an
// escape. The reader looked past the end of the read there (at what an
// earlier read left in its buffer) and lost the start of the line; the
// generated test found it now and then.
func TestEscapingReader_ChunkEndsAfterEscapedNewline(t *testing.T) {
	for name, chunks := range map[string][]string{
		"one read":                {"~\n~.x"},
		"split after ~\\n":        {"~\n", "~.x"},
		"split after ~":           {"~", "\n~.x"},
		"split after ~\\n~":       {"~\n~", ".x"},
		"every byte":              {"~", "\n", "~", ".", "x"},
		"after text and newline":  {"a\n~\n", "~.x"},
		"a stale ~ in the buffer": {"~~~~~~~~", "~\n", "y"},
	} {
		h := &testHandler{}
		var out bytes.Buffer
		if _, err := io.Copy(&out, NewReader(&chunkReader{chunks: append([]string(nil), chunks...)}, '~', h.handler)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := naiveEscapeCharacters(join(chunks), '~', (&testHandler{}).handler)
		if out.String() != want {
			t.Errorf("%s: read %q, want %q", name, out.String(), want)
		}
	}
}

func join(chunks []string) string {
	var b bytes.Buffer
	for _, c := range chunks {
		b.WriteString(c)
	}
	return b.String()
}
