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
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bufbuild/protocompile/reporter"
)

// countingAccessor returns an accessor that serves the given in-memory
// sources and records how many times each path is resolved.
func countingAccessor(srcs map[string]string) (func(string) (io.ReadCloser, error), func() map[string]int) {
	var mu sync.Mutex
	counts := map[string]int{}
	accessor := func(path string) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		counts[path]++
		src, ok := srcs[path]
		if !ok {
			return nil, fmt.Errorf("%s: %w", path, errors.New("not exist"))
		}
		return io.NopCloser(strings.NewReader(src)), nil
	}
	return accessor, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		snapshot := make(map[string]int, len(counts))
		for k, v := range counts {
			snapshot[k] = v
		}
		return snapshot
	}
}

// collectReporter collects reported errors and never aborts early.
type collectReporter struct {
	mu   sync.Mutex
	errs []reporter.ErrorWithPos
}

func (r *collectReporter) Error(err reporter.ErrorWithPos) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
	return nil
}

func (r *collectReporter) Warning(reporter.ErrorWithPos) {}

func (r *collectReporter) collected() []reporter.ErrorWithPos {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reporter.ErrorWithPos(nil), r.errs...)
}

// Two in-memory files that import each other (an import cycle). With
// MaxParallelism=1 the cycle is deterministically reported at the import
// statement in b.proto (line 2, column 8).
var cyclicSources = map[string]string{
	"a.proto": "syntax = \"proto3\";\nimport \"b.proto\";\nmessage A {}\n",
	"b.proto": "syntax = \"proto3\";\nimport \"a.proto\";\nmessage B {}\n",
}

func displayName(path string) (string, error) {
	return "display/" + path, nil
}

func TestCompileDisplayNameMapping(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("ErrorPositionsUseDisplayName", func(t *testing.T) {
		t.Parallel()
		accessor, counts := countingAccessor(cyclicSources)
		rep := &collectReporter{}
		compiler := Compiler{
			Resolver:          &SourceResolver{Accessor: accessor},
			Reporter:          rep,
			MaxParallelism:    1,
			DisplayNameMapper: displayName,
		}
		_, err := compiler.Compile(ctx, "a.proto")
		require.Error(t, err)

		// Exactly one diagnostic is reported, and its position uses the
		// display name, not the logical path.
		errs := rep.collected()
		require.Len(t, errs, 1)
		assert.Equal(t, "display/b.proto", errs[0].Start().Filename)
		assert.Equal(t, "display/b.proto:2:8", errs[0].GetPosition().String())
		assert.Contains(t, errs[0].Error(), "display/b.proto:2:8")
		assert.Contains(t, errs[0].Error(), "cycle found in imports")

		// The display name is never resolved or compiled as a new file:
		// each logical path is resolved exactly once and no display name
		// is ever passed to the resolver.
		resolved := counts()
		assert.Equal(t, 1, resolved["a.proto"])
		assert.Equal(t, 1, resolved["b.proto"])
		for path := range resolved {
			assert.NotContains(t, path, "display/")
		}
	})

	t.Run("MapperFailureAbortsWithCausedDiagnostic", func(t *testing.T) {
		t.Parallel()
		mapErr := errors.New("boom: cannot map path")
		accessor, _ := countingAccessor(map[string]string{
			"a.proto": "syntax = \"proto3\";\nimport \"b.proto\";\nmessage A { B b = 1; }\n",
			// syntax error in b.proto triggers a diagnostic for b.proto,
			// which triggers the (failing) display-name mapping.
			"b.proto": "syntax = \"proto3\";\nmessage B {\n",
		})
		compiler := Compiler{
			Resolver:       &SourceResolver{Accessor: accessor},
			MaxParallelism: 1,
			DisplayNameMapper: func(path string) (string, error) {
				if path == "b.proto" {
					return "", mapErr
				}
				return displayName(path)
			},
		}
		_, err := compiler.Compile(ctx, "a.proto")
		require.Error(t, err)
		// The failure is a diagnostic error (with position) whose cause
		// is the error returned by the mapping function.
		var errWithPos reporter.ErrorWithPos
		require.ErrorAs(t, err, &errWithPos)
		require.ErrorIs(t, err, mapErr)
		assert.Contains(t, err.Error(), `could not compute display name for path "b.proto"`)
	})

	t.Run("ResultsKeepLogicalPaths", func(t *testing.T) {
		t.Parallel()
		accessor, counts := countingAccessor(map[string]string{
			"a.proto": "syntax = \"proto3\";\nimport \"b.proto\";\nmessage A { B b = 1; }\n",
			"b.proto": "syntax = \"proto3\";\nmessage B {}\n",
		})
		compiler := Compiler{
			Resolver:          &SourceResolver{Accessor: accessor},
			MaxParallelism:    1,
			DisplayNameMapper: displayName,
		}
		files, err := compiler.Compile(ctx, "a.proto")
		require.NoError(t, err)
		require.Len(t, files, 1)
		// The compiled descriptor still uses the logical path, not the
		// display name.
		assert.Equal(t, "a.proto", files[0].Path())
		assert.Equal(t, 1, counts()["a.proto"])
		assert.Equal(t, 1, counts()["b.proto"])
	})
}

// TestCompileWithoutDisplayNameMapping is a regression check for the
// original interface: with no DisplayNameMapper configured, diagnostics
// report the same positions using the original logical paths.
func TestCompileWithoutDisplayNameMapping(t *testing.T) {
	t.Parallel()
	accessor, counts := countingAccessor(cyclicSources)
	rep := &collectReporter{}
	compiler := Compiler{
		Resolver:       &SourceResolver{Accessor: accessor},
		Reporter:       rep,
		MaxParallelism: 1,
	}
	_, err := compiler.Compile(t.Context(), "a.proto")
	require.Error(t, err)

	errs := rep.collected()
	require.Len(t, errs, 1)
	assert.Equal(t, "b.proto", errs[0].Start().Filename)
	assert.Equal(t, "b.proto:2:8", errs[0].GetPosition().String())
	assert.Contains(t, errs[0].Error(), "b.proto:2:8")
	assert.Contains(t, errs[0].Error(), "cycle found in imports")

	resolved := counts()
	assert.Equal(t, 1, resolved["a.proto"])
	assert.Equal(t, 1, resolved["b.proto"])
}
