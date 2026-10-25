// Copyright IBM Corp. 2021, 2025
// SPDX-License-Identifier: MPL-2.0

package helper

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Title returns the American English title format of s. A Caser keeps state
// and must not be shared between goroutines, so every call uses its own.
func Title(s string) string {
	return cases.Title(language.AmericanEnglish).String(s)
}
