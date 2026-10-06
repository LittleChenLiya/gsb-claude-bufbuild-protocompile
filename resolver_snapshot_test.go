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
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// mutatingResolver serves proto sources from a map, but returns different
// contents for a path on the second and subsequent lookups. It records how
// many times each path was actually read. It simulates an unstable resolver
// (e.g. a file that changes on disk mid-compilation).
type mutatingResolver struct {
	mu      sync.Mutex
	sources map[string]string
	calls   map[string]int
}

func newMutatingResolver(sources map[string]string) *mutatingResolver {
	return &mutatingResolver{sources: sources, calls: map[string]int{}}
}

func (r *mutatingResolver) FindFileByPath(path string) (SearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[path]++
	src, ok := r.sources[path]
	if !ok {
		return SearchResult{}, protoregistry.NotFound
	}
	if r.calls[path] > 1 {
		// Return a semantically different version on repeat reads.
		src = strings.Replace(src, "baz", "renamed_baz", -1)
		src = strings.Replace(src, "name", "renamed_name", -1)
	}
	return SearchResult{Source: strings.NewReader(src)}, nil
}

func (r *mutatingResolver) callCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[path]
}

// loadSnapshotTestSources loads the three small fixture protos
// (foo.proto -> bar.proto -> baz.proto) into a source map keyed by the
// import paths used inside the fixtures.
func loadSnapshotTestSources(t *testing.T) map[string]string {
	t.Helper()
	sources := make(map[string]string, 3)
	for _, name := range []string{"foo.proto", "bar.proto", "baz.proto"} {
		data, err := os.ReadFile(filepath.Join("internal", "testdata", "snapshot", name))
		require.NoError(t, err)
		sources["snapshot/"+name] = string(data)
	}
	return sources
}

func readSource(t *testing.T, res SearchResult) string {
	t.Helper()
	require.NotNil(t, res.Source)
	data, err := io.ReadAll(res.Source)
	require.NoError(t, err)
	return string(data)
}

func TestSnapshotResolverReadsPathOnlyOnce(t *testing.T) {
	t.Parallel()
	sources := loadSnapshotTestSources(t)
	underlying := newMutatingResolver(sources)

	// Sanity check: the fake resolver really does return different contents
	// for the same path on a second read.
	first, err := underlying.FindFileByPath("snapshot/bar.proto")
	require.NoError(t, err)
	second, err := underlying.FindFileByPath("snapshot/bar.proto")
	require.NoError(t, err)
	require.NotEqual(t, readSource(t, first), readSource(t, second),
		"fixture resolver must return different contents on repeat reads")

	// The snapshot reads the path only once and both lookups see identical
	// (first-read) contents.
	underlying = newMutatingResolver(sources)
	snapshot := &SnapshotResolver{Resolver: underlying}

	res1, err := snapshot.FindFileByPath("snapshot/bar.proto")
	require.NoError(t, err)
	content1 := readSource(t, res1)
	res2, err := snapshot.FindFileByPath("snapshot/bar.proto")
	require.NoError(t, err)
	content2 := readSource(t, res2)

	assert.Equal(t, sources["snapshot/bar.proto"], content1)
	assert.Equal(t, content1, content2, "repeat lookup must replay the first-read snapshot")
	assert.Equal(t, 1, underlying.callCount("snapshot/bar.proto"),
		"snapshot must read each path from the wrapped resolver exactly once")
}

func TestSnapshotResolverMissingFileConsistentlyFails(t *testing.T) {
	t.Parallel()
	underlying := newMutatingResolver(loadSnapshotTestSources(t))
	snapshot := &SnapshotResolver{Resolver: underlying}

	_, err1 := snapshot.FindFileByPath("snapshot/missing.proto")
	require.Error(t, err1)
	require.ErrorIs(t, err1, protoregistry.NotFound)
	_, err2 := snapshot.FindFileByPath("snapshot/missing.proto")
	require.Error(t, err2)
	assert.Equal(t, err1, err2, "missing path must fail the same way on every lookup")
	assert.Equal(t, 1, underlying.callCount("snapshot/missing.proto"))
}

func TestCompilerResolverSnapshotConsistency(t *testing.T) {
	t.Parallel()
	sources := loadSnapshotTestSources(t)
	underlying := newMutatingResolver(sources)
	compiler := Compiler{Resolver: underlying}

	files, err := compiler.Compile(t.Context(),
		"snapshot/foo.proto", "snapshot/bar.proto", "snapshot/baz.proto")
	require.NoError(t, err)
	require.Len(t, files, 3)

	// Each path was read exactly once during the whole compilation, so no
	// two references to the same path could observe different versions.
	for _, path := range []string{"snapshot/foo.proto", "snapshot/bar.proto", "snapshot/baz.proto"} {
		assert.Equal(t, 1, underlying.callCount(path), "path %s read more than once", path)
	}

	// Both references (the explicit file and the import target) are bound to
	// the same snapshotted contents, so the cross-file references line up.
	foo := files[0].Messages().ByName("Foo")
	require.NotNil(t, foo)
	barField := foo.Fields().ByName("bar")
	require.NotNil(t, barField)
	assert.Equal(t, protoreflect.FullName("snapshot.bar.Bar"), barField.Message().FullName())

	bar := files[1].Messages().ByName("Bar")
	require.NotNil(t, bar)
	bazField := bar.Fields().ByName("baz")
	require.NotNil(t, bazField)
	assert.Equal(t, protoreflect.FullName("snapshot.baz.Baz"), bazField.Message().FullName())
	assert.Same(t, files[2].Messages().ByName("Baz"), bazField.Message(),
		"import reference must resolve to the same descriptor as the explicitly compiled file")
}

func TestCompilerMissingImportFails(t *testing.T) {
	t.Parallel()
	sources := loadSnapshotTestSources(t)
	// bar.proto is absent, so foo.proto's normal import cannot be resolved.
	delete(sources, "snapshot/bar.proto")
	underlying := newMutatingResolver(sources)
	compiler := Compiler{Resolver: underlying}

	files, err := compiler.Compile(t.Context(), "snapshot/foo.proto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot/bar.proto")
	// No complete descriptors are delivered for the failed compilation.
	require.Len(t, files, 1)
	assert.Nil(t, files[0])
}
