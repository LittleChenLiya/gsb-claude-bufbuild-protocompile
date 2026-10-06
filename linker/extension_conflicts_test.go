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

package linker_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// linkExtConflictFixture parses and links the given fixture file (relative to
// the testdata directory). Each file is linked with its own symbol table (a
// nil symbols value), so linking alone cannot detect extension number
// conflicts across the resulting files.
func linkExtConflictFixture(t *testing.T, path string, deps linker.Files) linker.Result {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", filepath.FromSlash(path)))
	require.NoError(t, err)
	defer func() {
		require.NoError(t, f.Close())
	}()
	h := reporter.NewHandler(nil)
	fileAST, err := parser.Parse(path, f, h)
	require.NoError(t, err)
	parseResult, err := parser.ResultFromAST(fileAST, true, h)
	require.NoError(t, err)
	linkResult, err := linker.Link(parseResult, deps, nil, h)
	require.NoError(t, err)
	return linkResult
}

func TestCheckExtensionNumberConflicts(t *testing.T) {
	t.Parallel()

	base := linkExtConflictFixture(t, "extconflict/extendee.proto", nil)
	deps := linker.Files{base}
	// foo_ext_a.proto and foo_ext_b.proto both extend extconflict.Foo with
	// tag 100; foo_ext_c.proto extends Foo with tag 101; bar_ext_a.proto
	// extends extconflict.Bar with tag 100.
	fooA := linkExtConflictFixture(t, "extconflict/foo_ext_a.proto", deps)
	fooB := linkExtConflictFixture(t, "extconflict/foo_ext_b.proto", deps)
	fooC := linkExtConflictFixture(t, "extconflict/foo_ext_c.proto", deps)
	barA := linkExtConflictFixture(t, "extconflict/bar_ext_a.proto", deps)

	check := func(t *testing.T, files ...linker.File) (error, []reporter.ErrorWithPos) {
		t.Helper()
		var errs []reporter.ErrorWithPos
		rep := reporter.NewReporter(func(err reporter.ErrorWithPos) error {
			errs = append(errs, err)
			return nil
		}, nil)
		err := linker.CheckExtensionNumberConflicts(reporter.NewHandler(rep), files...)
		return err, errs
	}

	t.Run("SameExtendeeSameNumber", func(t *testing.T) {
		t.Parallel()
		err, errs := check(t, fooA, fooB)
		require.Error(t, err)
		require.Len(t, errs, 1)
		msg := errs[0].Error()
		// Both positions are reported: the conflicting declaration and the
		// first declaration of the same number for the same message.
		assert.Contains(t, msg, "extconflict/foo_ext_b.proto:8:27")
		assert.Contains(t, msg, "extension with tag 100 for message extconflict.Foo already defined at extconflict/foo_ext_a.proto:8:27")
	})

	t.Run("SameExtendeeDifferentNumbers", func(t *testing.T) {
		t.Parallel()
		err, errs := check(t, fooA, fooC)
		require.NoError(t, err)
		assert.Empty(t, errs)
	})

	t.Run("DifferentExtendeesSameNumber", func(t *testing.T) {
		t.Parallel()
		err, errs := check(t, fooA, barA)
		require.NoError(t, err)
		assert.Empty(t, errs)
	})

	t.Run("DuplicateFileIgnored", func(t *testing.T) {
		t.Parallel()
		// Passing the same file twice must not produce a conflict.
		err, errs := check(t, fooA, fooA)
		require.NoError(t, err)
		assert.Empty(t, errs)
	})
}
