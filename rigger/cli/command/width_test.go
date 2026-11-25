// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"testing"
	"unicode/utf8"
)

// A text of a table is one line of at most max characters: cut at a word
// when there is one in the second half, else in the word, and marked "...".
func TestFitText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		max      int
		expected string
	}{
		{name: "short", text: "One Redis server.", max: 60, expected: "One Redis server."},
		{name: "exactly the width", text: "abcde fghij", max: 11, expected: "abcde fghij"},
		{
			name: "cut at a word", text: "Port Redis listens on in the network namespace of the group (bridge mode).", max: 60,
			expected: "Port Redis listens on in the network namespace of the...",
		},
		{
			name: "punctuation before the cut", text: "One Redis server: a service, a password, a volume", max: 32,
			expected: "One Redis server: a service...",
		},
		{name: "one long word", text: "abcdefghijklmnopqrstuvwxyz", max: 10, expected: "abcdefg..."},
		{name: "lines and spaces", text: "first line\n  second\tline", max: 60, expected: "first line second line"},
		{name: "characters, not bytes", text: "árvíztűrő tükörfúrógép és még sok más szöveg", max: 20, expected: "árvíztűrő..."},
		{name: "no limit", text: "a b c d e f g h i j k l m n o p", max: 0, expected: "a b c d e f g h i j k l m n o p"},
		{name: "a width smaller than the mark", text: "abcdef", max: 2, expected: "ab"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := fitText(tt.text, tt.max)
			if got != tt.expected {
				t.Errorf("fitText(%q, %d) = %q, want %q", tt.text, tt.max, got, tt.expected)
			}

			if tt.max > 0 && utf8.RuneCountInString(got) > tt.max {
				t.Errorf("fitText(%q, %d) = %q: %d characters", tt.text, tt.max, got, utf8.RuneCountInString(got))
			}
		})
	}
}
