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

package importfix

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

func testResolver() protocompile.Resolver {
	return &protocompile.SourceResolver{ImportPaths: []string{"testdata"}}
}

func testIndex(entries ...[2]string) *SymbolIndex {
	idx := NewSymbolIndex()
	for _, e := range entries {
		idx.Add(e[0], e[1])
	}
	return idx
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + path)
	require.NoError(t, err)
	return data
}

func parseSource(path string, src []byte) (*ast.FileNode, error) {
	return parser.Parse(path, bytes.NewReader(src), reporter.NewHandler(nil))
}

// compileRevised compiles the revised source of the plan (overlaid on the
// testdata resolver) and returns the resulting file descriptor.
func compileRevised(t *testing.T, plan *Plan) protoreflect.FileDescriptor {
	t.Helper()
	require.NotNil(t, plan.RevisedSource)
	base := testResolver()
	overlay := protocompile.CompositeResolver{
		protocompile.ResolverFunc(func(name string) (protocompile.SearchResult, error) {
			if name == plan.Path {
				return protocompile.SearchResult{Source: bytes.NewReader(plan.RevisedSource)}, nil
			}
			return protocompile.SearchResult{}, os.ErrNotExist
		}),
		base,
	}
	compiler := protocompile.Compiler{Resolver: overlay}
	files, err := compiler.Compile(context.Background(), plan.Path)
	require.NoError(t, err)
	fd := files.FindFileByPath(plan.Path)
	require.NotNil(t, fd)
	return fd
}

// The three-file example: app/main.proto uses foo.Bar from foo/foo.proto
// (with middle/middle.proto also in the world of the index) but forgets
// the import. The unique candidate must be suggested.
func TestPlanUniqueMissingImport(t *testing.T) {
	t.Parallel()
	idx := testIndex(
		[2]string{"foo.Bar", "foo/foo.proto"},
		[2]string{"middle.Wrapper", "middle/middle.proto"},
	)
	planner := &Planner{Resolver: testResolver(), Index: idx}
	plan, err := planner.Plan(context.Background(), "app/main.proto")
	require.NoError(t, err)
	require.True(t, plan.Usable(), "plan should be usable: %+v (verify err: %v)", plan, plan.VerifyErr)
	assert.Equal(t, []string{"foo/foo.proto"}, plan.ImportsToAdd)
	assert.Empty(t, plan.Unresolved)
	assert.True(t, plan.Verified)

	// The revision must be a pure insertion: removing the inserted import
	// block yields the original bytes, so all comments and declarations
	// are preserved exactly.
	original := readFixture(t, "app/main.proto")
	revised := string(plan.RevisedSource)
	assert.Contains(t, revised, `import "foo/foo.proto";`)
	assert.Contains(t, revised, "// Uses foo.Bar but forgets")
	restored := strings.Replace(revised, "\nimport \"foo/foo.proto\";\n", "", 1)
	assert.Equal(t, string(original), restored, "revision must only insert the new import")

	// The revised source compiles and the field links to foo.Bar.
	fd := compileRevised(t, plan)
	msg := fd.Messages().ByName("Uses")
	require.NotNil(t, msg)
	field := msg.Fields().ByName("bar")
	require.NotNil(t, field)
	require.NotNil(t, field.Message())
	assert.Equal(t, protoreflect.FullName("foo.Bar"), field.Message().FullName())
}

// Two files defining the same symbol: the planner must not guess.
func TestPlanAmbiguousCandidates(t *testing.T) {
	t.Parallel()
	idx := testIndex(
		[2]string{"foo.Bar", "foo/foo.proto"},
		[2]string{"foo.Bar", "foo/alias.proto"},
	)
	planner := &Planner{Resolver: testResolver(), Index: idx}
	plan, err := planner.Plan(context.Background(), "app/main.proto")
	require.NoError(t, err)
	assert.False(t, plan.Usable())
	assert.Empty(t, plan.ImportsToAdd)
	assert.False(t, plan.Verified)
	assert.Nil(t, plan.RevisedSource, "no partial revision may be emitted")
	require.Len(t, plan.Unresolved, 1)
	un := plan.Unresolved[0]
	assert.Equal(t, "foo.Bar", un.Symbol)
	assert.Equal(t, ReasonAmbiguous, un.Reason)
	assert.Equal(t, []string{"foo/alias.proto", "foo/foo.proto"}, un.Candidates)
}

// A symbol already visible through an existing public import chain must be
// used as-is: no new import is suggested.
func TestPlanUsesExistingPublicImport(t *testing.T) {
	t.Parallel()
	idx := testIndex(
		[2]string{"foo.Bar", "foo/foo.proto"},
		[2]string{"middle.Wrapper", "middle/middle.proto"},
	)
	planner := &Planner{Resolver: testResolver(), Index: idx}
	plan, err := planner.Plan(context.Background(), "app/uses_middle.proto")
	require.NoError(t, err)
	require.True(t, plan.Usable())
	assert.Empty(t, plan.ImportsToAdd, "foo.Bar is already visible via the public import in middle/middle.proto")
	assert.Empty(t, plan.Unresolved)
	assert.Nil(t, plan.RevisedSource, "no changes needed")
}

// The only candidate would create an import cycle: mark it unresolved.
func TestPlanCycleRisk(t *testing.T) {
	t.Parallel()
	idx := testIndex([2]string{"cyc.Thing", "cyc/a.proto"})
	planner := &Planner{Resolver: testResolver(), Index: idx}
	plan, err := planner.Plan(context.Background(), "cyc/b.proto")
	require.NoError(t, err)
	assert.False(t, plan.Usable())
	assert.Empty(t, plan.ImportsToAdd)
	assert.Nil(t, plan.RevisedSource)
	require.Len(t, plan.Unresolved, 1)
	un := plan.Unresolved[0]
	assert.Equal(t, "cyc.Thing", un.Symbol)
	assert.Equal(t, ReasonCycle, un.Reason)
	assert.Equal(t, []string{"cyc/a.proto"}, un.Candidates)
}

// A symbol that the index does not know cannot be imported.
func TestPlanSymbolNotFound(t *testing.T) {
	t.Parallel()
	planner := &Planner{Resolver: testResolver(), Index: testIndex()}
	plan, err := planner.Plan(context.Background(), "app/unknown.proto")
	require.NoError(t, err)
	assert.False(t, plan.Usable())
	assert.Empty(t, plan.ImportsToAdd)
	assert.Nil(t, plan.RevisedSource)
	require.Len(t, plan.Unresolved, 1)
	un := plan.Unresolved[0]
	assert.Equal(t, "mystery.Thing", un.Symbol)
	assert.Equal(t, ReasonNotFound, un.Reason)
	assert.Empty(t, un.Candidates)
}

// PlanAST works on an already-parsed, unlinked AST and must produce the
// same plan as Plan.
func TestPlanAST(t *testing.T) {
	t.Parallel()
	idx := testIndex([2]string{"foo.Bar", "foo/foo.proto"})
	planner := &Planner{Resolver: testResolver(), Index: idx}
	viaPath, err := planner.Plan(context.Background(), "app/main.proto")
	require.NoError(t, err)

	src := readFixture(t, "app/main.proto")
	file, err := parseSource("app/main.proto", src)
	require.NoError(t, err)
	viaAST, err := planner.PlanAST(context.Background(), "app/main.proto", src, file)
	require.NoError(t, err)
	assert.Equal(t, viaPath.ImportsToAdd, viaAST.ImportsToAdd)
	assert.Equal(t, viaPath.Verified, viaAST.Verified)
	assert.Equal(t, viaPath.RevisedSource, viaAST.RevisedSource)
}

// Normal compilation does not auto-fix anything: compiling the broken file
// with the regular compiler still fails; only an explicit plan fixes it.
func TestNormalCompileDoesNotAutoFix(t *testing.T) {
	t.Parallel()
	compiler := protocompile.Compiler{Resolver: testResolver()}
	_, err := compiler.Compile(context.Background(), "app/main.proto")
	require.Error(t, err, "plain Compile must not repair missing imports")
}

func TestReasonString(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "not found", ReasonNotFound.String())
	assert.Equal(t, "ambiguous", ReasonAmbiguous.String())
	assert.Equal(t, "cycle risk", ReasonCycle.String())
}

// New imports land after the last existing import, and trailing comments on
// the import line stay put.
func TestInsertImportsAfterExistingImport(t *testing.T) {
	t.Parallel()
	src := []byte(`syntax = "proto3";

package app;

import "middle/middle.proto"; // trailing comment stays

message M {
  middle.Wrapper w = 1;
}
`)
	file, err := parseSource("app/m.proto", src)
	require.NoError(t, err)
	revised := string(insertImports(src, file, []string{"foo/foo.proto"}))
	expected := `syntax = "proto3";

package app;

import "middle/middle.proto"; // trailing comment stays
import "foo/foo.proto";

message M {
  middle.Wrapper w = 1;
}
`
	assert.Equal(t, expected, revised)
}

// SymbolIndex.AddFile records the symbols of a compiled file.
func TestSymbolIndexAddFile(t *testing.T) {
	t.Parallel()
	compiler := protocompile.Compiler{Resolver: testResolver()}
	files, err := compiler.Compile(context.Background(), "foo/foo.proto")
	require.NoError(t, err)
	idx := NewSymbolIndex()
	idx.AddFile(files.FindFileByPath("foo/foo.proto"))
	assert.Equal(t, []string{"foo/foo.proto"}, idx.Files("foo.Bar"))
	assert.Equal(t, []string{"foo/foo.proto"}, idx.Files(".foo.Bar.name"))
	assert.Empty(t, idx.Files("foo.Nope"))
}
