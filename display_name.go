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

// displayNameMappingReporter is a reporter.Reporter that rewrites the file
// name in the positions of reported diagnostics, using a mapper to translate
// logical source paths into display names. Only diagnostics are affected:
// import resolution, result caching, and the descriptors produced by a
// compilation all continue to use the logical paths.
type displayNameMappingReporter struct {
	mapper   func(path string) (string, error)
	delegate reporter.Reporter
}

func (r displayNameMappingReporter) Error(err reporter.ErrorWithPos) error {
	mapped, mapErr := r.mapDiagnostic(err)
	if mapErr != nil {
		// The display name could not be computed, so abort with a
		// diagnostic error whose cause is the mapping failure.
		return mapErr
	}
	if r.delegate == nil {
		return mapped
	}
	return r.delegate.Error(mapped)
}

func (r displayNameMappingReporter) Warning(err reporter.ErrorWithPos) {
	if r.delegate == nil {
		return
	}
	mapped, mapErr := r.mapDiagnostic(err)
	if mapErr != nil {
		// A warning cannot abort the operation, so if the display name
		// cannot be computed, report the warning with its original path.
		mapped = err
	}
	r.delegate.Warning(mapped)
}

func (r displayNameMappingReporter) mapDiagnostic(err reporter.ErrorWithPos) (reporter.ErrorWithPos, error) {
	start := err.Start()
	path := start.Filename
	if path == "" {
		return err, nil
	}
	displayName, mapErr := r.mapper(path)
	if mapErr != nil {
		return nil, reporter.Error(err, fmt.Errorf("could not compute display name for path %q: %w", path, mapErr))
	}
	if displayName == path {
		return err, nil
	}
	end := err.End()
	start.Filename = displayName
	if end.Filename == path {
		end.Filename = displayName
	}
	return reporter.Error(ast.NewSourceSpan(start, end), err), nil
}

var _ reporter.Reporter = displayNameMappingReporter{}
