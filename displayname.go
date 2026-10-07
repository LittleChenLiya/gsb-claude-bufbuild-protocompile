// Copyright 2020-2026 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package protocompile

import (
	"fmt"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/reporter"
)

// DisplayNameFunc maps the logical path of a Protobuf source file (the path
// that is used for import resolution, caching, and as the resulting file
// descriptor name) to a display name that is used when presenting
// diagnostics (errors and warnings) to the user.
//
// This mapping only affects the presentation of diagnostics. It does not
// affect import resolution, cache keys, descriptor names, or source code
// information: those always continue to use the original logical path. In
// particular, returning a different name for a file does not cause it to be
// compiled again as if it were a new file.
//
// If the function returns an empty string for a path that has no mapping, the
// original logical path is used as the display name. If it returns a non-nil
// error, compilation aborts with a diagnostic error (a reporter.ErrorWithPos)
// at the same source position that wraps the returned error as its cause. The
// function must be safe to invoke concurrently, since the compiler may report
// diagnostics from multiple goroutines.
type DisplayNameFunc func(logicalPath string) (displayName string, err error)

// withDisplayNames wraps the given reporter so that every diagnostic it sees
// is presented using a display name derived from fn (see DisplayNameFunc).
// If fn is nil, rep is returned unchanged. If rep is nil, a reporter that
// aborts on the first error is used, matching the behavior of a Compiler with
// no configured reporter.
func withDisplayNames(rep reporter.Reporter, fn DisplayNameFunc) reporter.Reporter {
	if fn == nil {
		return rep
	}
	if rep == nil {
		rep = reporter.NewReporter(nil, nil)
	}
	return &displayNameReporter{delegate: rep, displayName: fn}
}

type displayNameReporter struct {
	delegate    reporter.Reporter
	displayName DisplayNameFunc
}

func (r *displayNameReporter) Error(err reporter.ErrorWithPos) error {
	logicalPath := err.GetPosition().Filename
	name, mapErr := r.displayName(logicalPath)
	if mapErr != nil {
		// The mapping function failed. Report (and abort with) a diagnostic
		// error at the same source position that wraps the failure as cause.
		diag := reporter.Error(
			err,
			fmt.Errorf("could not resolve display name for %q: %w", logicalPath, mapErr),
		)
		_ = r.delegate.Error(diag)
		return diag
	}
	if name == "" {
		name = logicalPath
	}
	return r.delegate.Error(rewriteError(err, name))
}

func (r *displayNameReporter) Warning(err reporter.ErrorWithPos) {
	logicalPath := err.GetPosition().Filename
	name, mapErr := r.displayName(logicalPath)
	if mapErr != nil {
		// A warning must never fail a compilation, so if the mapping function
		// failed we fall back to the logical path instead of dropping the
		// warning.
		name = logicalPath
	} else if name == "" {
		name = logicalPath
	}
	r.delegate.Warning(rewriteError(err, name))
}

// rewriteError returns a copy of err whose position uses the given file name
// while preserving the error's line, column, span, and underlying cause.
func rewriteError(err reporter.ErrorWithPos, filename string) reporter.ErrorWithPos {
	return reporter.Error(renameSpan(err, filename), err.Unwrap())
}

// renameSpan returns a span identical to span except that both its start and
// end positions reference the given file name.
func renameSpan(span ast.SourceSpan, filename string) ast.SourceSpan {
	start := span.Start()
	end := span.End()
	if start.Filename == filename && end.Filename == filename {
		return span
	}
	start.Filename = filename
	end.Filename = filename
	return ast.NewSourceSpan(start, end)
}
