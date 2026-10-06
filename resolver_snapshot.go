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
	"bytes"
	"io"
	"sync"
)

// SnapshotResolver is a Resolver that wraps another resolver and records a
// snapshot of each file the first time the file's path is resolved. Subsequent
// queries for the same path return the same result as the first query, without
// consulting the underlying resolver again.
//
// This guarantees that a single compilation observes a single, consistent
// version of every file: a logical import is bound to the contents that were
// first read for its path, and all other references to that same path re-use
// those contents. So one compilation can never mix different versions of a
// file, even if the underlying resolver would return different contents for
// the same path on subsequent calls.
//
// If a query of the underlying resolver fails, that error is also part of the
// snapshot: subsequent queries for the same path fail in the same way. So a
// missing import deterministically fails the compilation.
//
// SnapshotResolver is safe for concurrent use. A single SnapshotResolver
// should not be shared by unrelated compilation operations unless it is
// acceptable for them to observe the exact same file contents. The Compiler
// automatically uses a per-compilation snapshot (see Compiler.Resolver), so
// explicitly wrapping a resolver in a SnapshotResolver is usually unnecessary.
type SnapshotResolver struct {
	// Resolver is the underlying resolver, consulted at most once per path.
	Resolver Resolver

	mu      sync.Mutex
	entries map[string]snapshotEntry
}

var _ Resolver = (*SnapshotResolver)(nil)

// NewSnapshotResolver returns a Resolver that resolves paths using the given
// resolver, but records a snapshot of the first result for each path and
// re-uses that snapshot for all subsequent queries of the same path.
func NewSnapshotResolver(r Resolver) *SnapshotResolver {
	return &SnapshotResolver{Resolver: r}
}

// snapshotEntry is the recorded result of resolving a single path.
type snapshotEntry struct {
	result SearchResult
	// source holds the materialized contents of result.Source, if the
	// underlying resolver returned source for this path. In that case,
	// result.Source is nil and a fresh reader over source is returned for
	// every query, so all callers observe identical contents.
	source    []byte
	hasSource bool
	err       error
}

func (e snapshotEntry) searchResult() SearchResult {
	res := e.result
	if e.hasSource {
		res.Source = bytes.NewReader(e.source)
	}
	return res
}

// FindFileByPath implements the Resolver interface.
func (r *SnapshotResolver) FindFileByPath(path string) (SearchResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]snapshotEntry{}
	}
	if entry, ok := r.entries[path]; ok {
		return entry.searchResult(), entry.err
	}

	res, err := r.Resolver.FindFileByPath(path)
	entry := snapshotEntry{result: res, err: err}
	if err == nil && res.Source != nil {
		// Materialize the source now so that the snapshot holds the actual
		// contents (a reader can only be consumed once) and so that every
		// query for this path observes the exact same bytes.
		data, readErr := io.ReadAll(res.Source)
		if closer, ok := res.Source.(io.Closer); ok {
			_ = closer.Close()
		}
		if readErr != nil {
			entry = snapshotEntry{err: readErr}
		} else {
			entry.source = data
			entry.hasSource = true
			entry.result.Source = nil
		}
	}
	r.entries[path] = entry
	return entry.searchResult(), entry.err
}
