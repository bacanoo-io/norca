// Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build race

package scheduler

// raceDetector reports whether the tests run with the race detector, under
// which the scheduler is several times slower: a limit on how long the
// scheduler may take says nothing there.
const raceDetector = true
