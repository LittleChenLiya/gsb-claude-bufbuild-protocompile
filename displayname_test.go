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
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bufbuild/protocompile/reporter"
)

// countingResolver records how many times each logical path is resolved. This
// lets us prove that display-name renaming neither changes the keys used for
// import resolution/caching nor causes a file to be compiled a second time.
type countingResolver struct {
	mu      sync.Mutex
	sources map[string]string
	calls   map[string]int
}

func (r *countingResolver) FindFileByPath(path string) (SearchResult, error) {
	r.mu.Lock()
	r.calls[path]++
	src, ok := r.sources[path]
	r.mu.Unlock()
	if !ok {
		return SearchResult{}, os.ErrNotExist
	}
	return SearchResult{Source: strings.NewReader(src)}, nil
}

func (r *countingResolver) callCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[path]
}

// mutually importing in-memory files:
//
//	a.proto imports b.proto and vice-versa, which is an import cycle. The
//	cycle is reported at the importing statement of whichever file the
//	scheduler links first (a.proto line 3 col 8, or b.proto line 2 col 8);
//	the tests accept either side.
var cycleSources = map[string]string{
	"a.proto": `syntax = "proto3";

import "b.proto";
message A {}
`,
	"b.proto": `syntax = "proto3";
import "a.proto";
message B {}
`,
}

var errDisplayNameBoom = errors.New("display-name mapping exploded")

// makeDisplayNames maps a.proto -> a-display.proto and b.proto ->
// b-display.proto. Each entry in failFor makes the mapping return an error for
// that logical path.
func makeDisplayNames(failFor ...string) DisplayNameFunc {
	fail := map[string]struct{}{}
	for _, f := range failFor {
		fail[f] = struct{}{}
	}
	return func(logicalPath string) (string, error) {
		if _, ok := fail[logicalPath]; ok {
			return "", errDisplayNameBoom
		}
		switch logicalPath {
		case "a.proto":
			return "a-display.proto", nil
		case "b.proto":
			return "b-display.proto", nil
		default:
			return "", nil
		}
	}
}

// compileCycleInMemory runs the mutually-importing in-memory fixture with the
// given display-name function and returns the error (if any) and the resolver.
func compileCycleInMemory(t *testing.T, fn DisplayNameFunc) (error, *countingResolver) {
	t.Helper()
	res := &countingResolver{sources: cycleSources, calls: map[string]int{}}
	c := Compiler{
		Resolver:       res,
		DisplayName:    fn,
		MaxParallelism: 1,
	}
	_, err := c.Compile(context.Background(), "a.proto")
	return err, res
}

// assertCycleDiagnostic verifies that err is the import-cycle diagnostic and
// returns the logical path of the file at whose import statement the cycle is
// reported. Depending on goroutine scheduling, either side of the cycle may
// be reported first, but both sides are valid:
//
//   - a.proto's import is on line 3, column 8; the cycle text is
//     "a.proto" -> "b.proto" -> "a.proto"
//   - b.proto's import is on line 2, column 8; the cycle text is
//     "b.proto" -> "a.proto" -> "b.proto"
func assertCycleDiagnostic(t *testing.T, err error) string {
	t.Helper()
	var ewp reporter.ErrorWithPos
	require.ErrorAs(t, err, &ewp)
	pos := ewp.GetPosition()
	assert.Equal(t, 8, pos.Col, "error column should pin to import string, got %v", err)

	var logical string
	switch pos.Filename {
	case "a-display.proto":
		logical = "a.proto"
		assert.Equal(t, 3, pos.Line)
		assert.Contains(t, err.Error(), `"a.proto" -> "b.proto" -> "a.proto"`)
		assert.True(t, strings.HasPrefix(err.Error(), "a-display.proto:3:8: "), "got %v", err)
	case "b-display.proto":
		logical = "b.proto"
		assert.Equal(t, 2, pos.Line)
		assert.Contains(t, err.Error(), `"b.proto" -> "a.proto" -> "b.proto"`)
		assert.True(t, strings.HasPrefix(err.Error(), "b-display.proto:2:8: "), "got %v", err)
	default:
		t.Fatalf("unexpected error position/name %q: %v", pos.Filename, err)
	}
	return logical
}

// TestDisplayNameMappingInMemory exercises the feature entirely with
// in-memory files: two files that import each other.
func TestDisplayNameMappingInMemory(t *testing.T) {
	t.Parallel()

	// display name appears in the diagnostic, but resolution/caching still
	// use logical paths and each file is compiled exactly once.
	t.Run("mapped", func(t *testing.T) {
		t.Parallel()
		err, res := compileCycleInMemory(t, makeDisplayNames())
		require.Error(t, err)

		reported := assertCycleDiagnostic(t, err)

		// each file resolved exactly once: the rename did not spawn a new compile
		assert.Equal(t, 1, res.callCount("a.proto"))
		assert.Equal(t, 1, res.callCount("b.proto"))
		// the display names must never be passed to the resolver
		assert.Equal(t, 0, res.callCount("a-display.proto"))
		assert.Equal(t, 0, res.callCount("b-display.proto"))
		// sanity: the reported side's logical name is one of the two files
		assert.Contains(t, []string{"a.proto", "b.proto"}, reported)
	})

	// short regression for the original interface: with no mapping configured,
	// the output is exactly the pre-feature output.
	t.Run("mapping_disabled_original_output", func(t *testing.T) {
		t.Parallel()
		err, res := compileCycleInMemory(t, nil)
		require.Error(t, err)

		var ewp reporter.ErrorWithPos
		require.ErrorAs(t, err, &ewp)
		pos := ewp.GetPosition()
		assert.Equal(t, 8, pos.Col)
		switch pos.Filename {
		case "a.proto":
			assert.Equal(t, 3, pos.Line)
			assert.True(t, strings.HasPrefix(err.Error(), "a.proto:3:8: "), "got %v", err)
			assert.Contains(t, err.Error(), `"a.proto" -> "b.proto" -> "a.proto"`)
		case "b.proto":
			assert.Equal(t, 2, pos.Line)
			assert.True(t, strings.HasPrefix(err.Error(), "b.proto:2:8: "), "got %v", err)
			assert.Contains(t, err.Error(), `"b.proto" -> "a.proto" -> "b.proto"`)
		default:
			t.Fatalf("unexpected error file %q: %v", pos.Filename, err)
		}

		// same resolver-call profile as the mapped case
		assert.Equal(t, 1, res.callCount("a.proto"))
		assert.Equal(t, 1, res.callCount("b.proto"))
		assert.Equal(t, 0, res.callCount("a-display.proto"))
	})

	// a failing mapping function aborts compilation with a diagnostic error
	// at the same position that wraps the failure as its cause. The mapping
	// fails for both files so this outcome is independent of which side of the
	// cycle is reported first.
	t.Run("mapping_failure", func(t *testing.T) {
		t.Parallel()
		err, _ := compileCycleInMemory(t, makeDisplayNames("a.proto", "b.proto"))
		require.Error(t, err)

		var ewp reporter.ErrorWithPos
		require.ErrorAs(t, err, &ewp)
		pos := ewp.GetPosition()
		// on failure the logical path is retained in the position prefix
		assert.Contains(t, []string{"a.proto", "b.proto"}, pos.Filename)
		assert.Equal(t, 8, pos.Col)
		if pos.Filename == "a.proto" {
			assert.Equal(t, 3, pos.Line)
		} else {
			assert.Equal(t, 2, pos.Line)
		}

		// the failure cause is reachable via errors.Is
		assert.True(t, errors.Is(err, errDisplayNameBoom), "expected cause to be wrapped, got %v", err)
		assert.Contains(t, err.Error(), `could not resolve display name for "`)
		assert.Contains(t, err.Error(), errDisplayNameBoom.Error())
	})

	// a single invalid file deterministically pins the mapping-failure
	// diagnostic to the bad token and keeps the logical path.
	t.Run("mapping_failure_position", func(t *testing.T) {
		t.Parallel()
		src := strings.NewReader(`syntax = "proto3";
message M { bad }
`)
		c := Compiler{
			Resolver:       ResolverFunc(func(string) (SearchResult, error) { return SearchResult{Source: src}, nil }),
			DisplayName:    makeDisplayNames("x.proto"),
			MaxParallelism: 1,
		}
		_, err := c.Compile(context.Background(), "x.proto")
		require.Error(t, err)

		var ewp reporter.ErrorWithPos
		require.ErrorAs(t, err, &ewp)
		pos := ewp.GetPosition()
		assert.Equal(t, "x.proto", pos.Filename)
		assert.Equal(t, 2, pos.Line)
		assert.True(t, errors.Is(err, errDisplayNameBoom), "got %v", err)
		assert.Contains(t, err.Error(), `could not resolve display name for "x.proto"`)
	})
}

// countingAccessor is an os-backed file accessor that records the logical
// paths it was asked to open.
type countingAccessor struct {
	mu    sync.Mutex
	calls map[string]int
}

func (a *countingAccessor) open(path string) (io.ReadCloser, error) {
	a.mu.Lock()
	a.calls[path]++
	a.mu.Unlock()
	return os.Open(path)
}

// TestDisplayNameReporterIntegration checks that a custom Reporter receives
// diagnostics whose file name is the display name, and that a mapping failure
// reaches that reporter as a position-carrying error wrapping the cause.
func TestDisplayNameReporterIntegration(t *testing.T) {
	t.Parallel()

	// a parse error is delivered to the configured reporter
	const badSource = `syntax = "proto3";
message M { !!! }
`

	resolveA := func(path string) (SearchResult, error) {
		if path != "a.proto" {
			return SearchResult{}, os.ErrNotExist
		}
		return SearchResult{Source: strings.NewReader(badSource)}, nil
	}

	t.Run("mapped error delivered to reporter", func(t *testing.T) {
		t.Parallel()
		var got reporter.ErrorWithPos
		rep := reporter.NewReporter(
			func(e reporter.ErrorWithPos) error { got = e; return nil }, // collect, keep compiling
			nil,
		)
		c := Compiler{
			Resolver:       ResolverFunc(resolveA),
			Reporter:       rep,
			DisplayName:    makeDisplayNames(),
			MaxParallelism: 1,
		}
		_, err := c.Compile(context.Background(), "a.proto")
		require.Error(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "a-display.proto", got.GetPosition().Filename)
		assert.Contains(t, got.Error(), "a-display.proto")
	})

	t.Run("mapping failure delivered to reporter with cause", func(t *testing.T) {
		t.Parallel()
		var got reporter.ErrorWithPos
		rep := reporter.NewReporter(
			func(e reporter.ErrorWithPos) error { got = e; return nil },
			nil,
		)
		c := Compiler{
			Resolver:       ResolverFunc(resolveA),
			Reporter:       rep,
			DisplayName:    makeDisplayNames("a.proto"),
			MaxParallelism: 1,
		}
		_, err := c.Compile(context.Background(), "a.proto")
		require.Error(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "a.proto", got.GetPosition().Filename)
		assert.True(t, errors.Is(got, errDisplayNameBoom))
	})
}

// TestDisplayNameMappingFromTempDir repeats the same scenario but resolves the
// files from a small temporary directory on disk via SourceResolver.
func TestDisplayNameMappingFromTempDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for name, src := range cycleSources {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644))
	}

	accessor := &countingAccessor{calls: map[string]int{}}
	c := Compiler{
		Resolver: &SourceResolver{
			ImportPaths: []string{dir},
			Accessor:    accessor.open,
		},
		DisplayName:    makeDisplayNames(),
		MaxParallelism: 1,
	}
	_, err := c.Compile(context.Background(), "a.proto")
	require.Error(t, err)

	var ewp reporter.ErrorWithPos
	require.ErrorAs(t, err, &ewp)
	pos := ewp.GetPosition()
	assert.Equal(t, 8, pos.Col)
	switch pos.Filename {
	case "a-display.proto":
		assert.Equal(t, 3, pos.Line)
		assert.Contains(t, err.Error(), `"a.proto" -> "b.proto" -> "a.proto"`)
		assert.True(t, strings.HasPrefix(err.Error(), "a-display.proto:3:8: "), "got %v", err)
	case "b-display.proto":
		assert.Equal(t, 2, pos.Line)
		assert.Contains(t, err.Error(), `"b.proto" -> "a.proto" -> "b.proto"`)
		assert.True(t, strings.HasPrefix(err.Error(), "b-display.proto:2:8: "), "got %v", err)
	default:
		t.Fatalf("unexpected error file %q: %v", pos.Filename, err)
	}

	// only the two logical paths touch the file system, each exactly once;
	// the display names are never used for resolution (the compiler may also
	// probe google/protobuf/descriptor.proto once through the accessor)
	assert.Equal(t, 1, accessor.calls[filepath.Join(dir, "a.proto")])
	assert.Equal(t, 1, accessor.calls[filepath.Join(dir, "b.proto")])
	assert.Equal(t, 0, accessor.calls[filepath.Join(dir, "a-display.proto")])
	assert.Equal(t, 0, accessor.calls[filepath.Join(dir, "b-display.proto")])
}
