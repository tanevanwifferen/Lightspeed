// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bug is a minimal stand-in for gopls's internal/util/bug
// package, providing only what the vendored protocol code uses.
// The original reports telemetry counters; this version simply
// formats an error.
package bug

import "fmt"

// Errorf calls fmt.Errorf and reports the resulting error as a bug.
// In this trimmed-down vendored copy, no report is filed; the error
// is returned as-is.
func Errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
