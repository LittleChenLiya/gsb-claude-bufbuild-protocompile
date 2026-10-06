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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVersioningResolver returns different contents each time it is asked
// for the same path, and records how many times each path was requested.
type fakeVersioningResolver struct {
	mu    sync.Mutex
	calls map[string]int
}

func (r *fakeVersioningResolver) FindFileByPath(path string) (SearchResult, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[path]++
	n := r.calls[path]
	r.mu.Unlock()
	src := fmt.Sprintf(`syntax = "proto3"; message M%d { int32 v = 1; }`, n)
	return SearchResult{Source: strings.NewReader(src)}, nil
}

func (r *fakeVersioningResolver) callCount(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[path]
}

// The snapshot must read each path only once, and all references to the same
// path must observe identical contents, even when the underlying resolver
// would return a different version on a second read.
func TestSnapshotResolverReadsPathOnce(t *testing.T) {
	t.Parallel()
	base := &fakeVersioningResolver{}
	snapshot := NewSnapshotResolver(base)

	first, err := snapshot.FindFileByPath("foo.proto")
	require.NoError(t, err)
	second, err := snapshot.FindFileByPath("foo.proto")
	require.NoError(t, err)

	// The underlying resolver was consulted exactly once for the path.
	assert.Equal(t, 1, base.callCount("foo.proto"))

	// Both references observe the exact same contents.
	contents1, err := io.ReadAll(first.Source)
	require.NoError(t, err)
	contents2, err := io.ReadAll(second.Source)
	require.NoError(t, err)
	assert.Equal(t, string(contents1), string(contents2))
	assert.Contains(t, string(contents1), "message M1 ")
}

// A compilation that references the same file from two places (directly and
// transitively) must resolve each path only once and link both references to
// the same descriptor.
func TestCompileSnapshotsResolverContents(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := map[string]int{}
	base := ResolverFunc(func(path string) (SearchResult, error) {
		mu.Lock()
		calls[path]++
		mu.Unlock()
		file, err := os.Open(filepath.Join("internal", "testdata", path))
		if err != nil {
			return SearchResult{}, err
		}
		return SearchResult{Source: file}, nil
	})
	compiler := Compiler{Resolver: base}

	files, err := compiler.Compile(context.Background(), "snapshot/snapshot_a.proto")
	require.NoError(t, err)
	require.Len(t, files, 1)

	// Each file was resolved exactly once during the compilation, even
	// though snapshot_c.proto is referenced from two places.
	assert.Equal(t, 1, calls["snapshot/snapshot_a.proto"])
	assert.Equal(t, 1, calls["snapshot/snapshot_b.proto"])
	assert.Equal(t, 1, calls["snapshot/snapshot_c.proto"])

	// The direct import of snapshot_c.proto and the transitive one (via
	// snapshot_b.proto) are the very same descriptor: one version only.
	a := files[0]
	require.Equal(t, 2, a.Imports().Len())
	b := a.Imports().Get(0)
	cDirect := a.Imports().Get(1)
	require.Equal(t, "snapshot/snapshot_b.proto", b.Path())
	require.Equal(t, "snapshot/snapshot_c.proto", cDirect.Path())
	require.Equal(t, 1, b.Imports().Len())
	cViaB := b.Imports().Get(0)
	assert.True(t, cDirect == cViaB, "expected both references to snapshot_c.proto to be the same descriptor")
}

// A missing regular import must clearly fail the compilation, and no
// complete descriptors are delivered for the failed file.
func TestCompileMissingImportFails(t *testing.T) {
	t.Parallel()
	accessor := SourceAccessorFromMap(map[string]string{
		"snapshot/needs_missing.proto": `
			syntax = "proto3";
			package snapshot;
			import "snapshot/does_not_exist.proto";
			message NeedsMissing {
			  int32 value = 1;
			}
		`,
	})
	compiler := Compiler{Resolver: &SourceResolver{Accessor: accessor}}

	files, err := compiler.Compile(context.Background(), "snapshot/needs_missing.proto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "snapshot/does_not_exist.proto")

	// No complete descriptors are delivered for the failed compilation.
	require.Len(t, files, 1)
	assert.Nil(t, files[0])
}
