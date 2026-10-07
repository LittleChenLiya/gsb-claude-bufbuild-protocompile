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
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// In-memory fixtures for the small three-file example:
//
//   - foo/bar.proto defines package foo and message foo.Bar: the unique
//     match for a reference to foo.Bar.
//   - other/bar.proto defines package other and message other.Bar: a
//     second symbol with the same short name "Bar", used to prove that
//     ambiguous matches are not guessed.
//   - public/public.proto publicly re-exports foo/bar.proto, used to
//     prove reuse of an already-public import path.
const importRevisionFooBarProto = `syntax = "proto3";

package foo;

// Bar is the one and only foo.Bar; this comment must never be rewritten.
message Bar {
  int32 value = 1;
}
`

const importRevisionOtherBarProto = `syntax = "proto3";

package other;

message Bar {
  string value = 1;
}
`

const importRevisionPublicProto = `syntax = "proto3";

package publicpkg;

// re-export foo.Bar publicly so importers of this file see it too
import public "foo/bar.proto";

message PublicOwn {
}
`

func mapResolver(srcs map[string]string) Resolver {
	return ResolverFunc(func(path string) (SearchResult, error) {
		s, ok := srcs[path]
		if !ok {
			return SearchResult{}, os.ErrNotExist
		}
		return SearchResult{Source: strings.NewReader(s)}, nil
	})
}

func basicRevisionResolver() Resolver {
	return mapResolver(map[string]string{
		"foo/bar.proto":   importRevisionFooBarProto,
		"other/bar.proto": importRevisionOtherBarProto,
	})
}

func parseUnlinked(t *testing.T, name, source string) parser.Result {
	t.Helper()
	h := reporter.NewHandler(nil)
	file, err := parser.Parse(name, strings.NewReader(source), h)
	require.NoError(t, err)
	result, err := parser.ResultFromAST(file, true, h)
	require.NoError(t, err)
	return result
}

func findReference(plan *ImportRevisionPlan, name string, kind TypeReferenceKind) (ReferenceResolution, bool) {
	for _, r := range plan.References {
		if r.Reference.Name == name && r.Reference.Kind == kind {
			return r, true
		}
	}
	return ReferenceResolution{}, false
}

func TestPlanImportRevisions_UniqueFooBar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := basicRevisionResolver()
	idx, err := NewSymbolFileIndex(ctx, resolver, "foo/bar.proto")
	require.NoError(t, err)

	targetSrc := `syntax = "proto3";

package consumer;

// leading comment must survive planning
message UsesBar {
  foo.Bar bar = 1; // trailing comment
}
`
	unlinked := parseUnlinked(t, "consumer.proto", targetSrc)
	before := append([]string(nil), unlinked.FileDescriptorProto().GetDependency()...)

	// The file genuinely fails to link without the import.
	_, linkErr := (&Compiler{Resolver: resolver}).Compile(ctx, "consumer.proto")
	require.Error(t, linkErr)

	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	require.True(t, plan.Usable(), "plan should be usable: status=%s notes=%v", plan.Status, plan.Notes)
	assert.Equal(t, ImportRevisionStatusUnique, plan.Status)
	assert.Equal(t, []string{"foo/bar.proto"}, plan.AddImports)

	// The reference must have been taken from the AST, not error text.
	res, ok := findReference(plan, "foo.Bar", TypeReferenceKindField)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusUnique, res.Status)
	assert.Equal(t, []string{"foo/bar.proto"}, res.CandidateFiles)

	// The original parse result is untouched: no dependency added.
	assert.Equal(t, before, unlinked.FileDescriptorProto().GetDependency())
	// Original comments and declarations are unchanged.
	fileNode := unlinked.AST()
	require.NotNil(t, fileNode)
	var msg *ast.MessageNode
	for _, decl := range fileNode.Decls {
		if m, ok := decl.(*ast.MessageNode); ok {
			msg = m
			break
		}
	}
	require.NotNil(t, msg)
	msgInfo := fileNode.NodeInfo(msg)
	require.NotNil(t, msgInfo.LeadingComments())
	require.GreaterOrEqual(t, msgInfo.LeadingComments().Len(), 1)
	assert.Contains(t, msgInfo.LeadingComments().Index(0).RawText(), "leading comment must survive planning")
	field := msg.MessageBody.Decls[0].(*ast.FieldNode)
	fieldInfo := fileNode.NodeInfo(field)
	require.NotNil(t, fieldInfo.TrailingComments())
	require.GreaterOrEqual(t, fieldInfo.TrailingComments().Len(), 1)
	assert.Contains(t, fieldInfo.TrailingComments().Index(0).RawText(), "trailing comment")

	// Applying the revision produces a descriptor that links.
	linked, err := compileWithExtraImports(ctx, resolver, unlinked, plan.AddImports, nil)
	require.NoError(t, err)
	assert.Equal(t, "consumer.proto", linked.Path())
	fieldDesc := linked.Messages().Get(0).Fields().Get(0)
	require.NotNil(t, fieldDesc.Message())
	assert.Equal(t, "foo.Bar", string(fieldDesc.Message().FullName()))
}

func TestPlanImportRevisions_AmbiguousSameShortName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := basicRevisionResolver()
	idx, err := NewSymbolFileIndex(ctx, resolver, "foo/bar.proto", "other/bar.proto")
	require.NoError(t, err)

	// An unqualified reference with two same-short-name candidates must
	// not be guessed.
	targetSrc := `syntax = "proto3";

package consumer;

message UsesBar {
  Bar bar = 1;
}
`
	unlinked := parseUnlinked(t, "consumer.proto", targetSrc)
	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	require.False(t, plan.Usable())
	assert.Equal(t, ImportRevisionStatusAmbiguous, plan.Status)
	assert.Empty(t, plan.AddImports)
	res, ok := findReference(plan, "Bar", TypeReferenceKindField)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusAmbiguous, res.Status)
	assert.ElementsMatch(t, []string{"foo/bar.proto", "other/bar.proto"}, res.CandidateFiles)

	// Ordinary Compile reports the error and does not auto-fix anything.
	_, linkErr := (&Compiler{Resolver: resolver}).Compile(ctx, "consumer.proto")
	require.Error(t, linkErr)
}

func TestPlanImportRevisions_ReusesExistingPublicImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := mapResolver(map[string]string{
		"foo/bar.proto":       importRevisionFooBarProto,
		"public/public.proto": importRevisionPublicProto,
	})
	idx, err := NewSymbolFileIndex(ctx, resolver, "foo/bar.proto", "public/public.proto")
	require.NoError(t, err)

	targetSrc := `syntax = "proto3";

package consumer;

import "public/public.proto";

message UsesBar {
  foo.Bar bar = 1;
}
`
	resolver = mapResolver(map[string]string{
		"foo/bar.proto":       importRevisionFooBarProto,
		"public/public.proto": importRevisionPublicProto,
		"consumer.proto":      targetSrc,
	})
	unlinked := parseUnlinked(t, "consumer.proto", targetSrc)
	// Already links thanks to the transitive public import.
	files, linkErr := (&Compiler{Resolver: resolver}).Compile(ctx, "consumer.proto")
	require.NoError(t, linkErr)
	require.Len(t, files, 1)

	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	assert.True(t, plan.Usable())
	assert.Empty(t, plan.AddImports, "existing public import path should be reused, not duplicated")
	res, ok := findReference(plan, "foo.Bar", TypeReferenceKindField)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusResolved, res.Status)
}

func TestPlanImportRevisions_UnresolvedSymbol(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := basicRevisionResolver()
	idx, err := NewSymbolFileIndex(ctx, resolver, "foo/bar.proto")
	require.NoError(t, err)

	targetSrc := `syntax = "proto3";

package consumer;

message UsesMissing {
  does.not.Exist missing = 1;
}
`
	unlinked := parseUnlinked(t, "consumer.proto", targetSrc)
	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	assert.False(t, plan.Usable())
	assert.Equal(t, ImportRevisionStatusUnresolved, plan.Status)
	assert.Empty(t, plan.AddImports)
	res, ok := findReference(plan, "does.not.Exist", TypeReferenceKindField)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusUnresolved, res.Status)
}

func TestPlanImportRevisions_ImportCycleMarkedUndecided(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// candidateA.proto (pkgA.Widget) imports leaf.proto, which imports
	// target.proto. So suggesting "import candidateA.proto" inside
	// target.proto would create a cycle.
	srcs := map[string]string{
		"candidateA.proto": `syntax = "proto3";

package pkga;

import "leaf.proto";

message Widget {
  int32 value = 1;
}
`,
		"leaf.proto": `syntax = "proto3";

package leafpkg;

import "target.proto";

message Leaf {
  pkgtarget.Anchor anchor = 1;
}
`,
		"target.proto": `syntax = "proto3";

package pkgtarget;

message Anchor {
  int32 value = 1;
}
`,
	}
	resolver := mapResolver(srcs)
	idx, err := NewSymbolFileIndex(ctx, resolver, "candidateA.proto", "leaf.proto", "target.proto")
	require.NoError(t, err)

	// Revised target: references pkgA.Widget without the import.
	targetRevisionSrc := `syntax = "proto3";

package pkgtarget;

message Anchor {
  int32 value = 1;
}

message UsesWidget {
  pkga.Widget widget = 1;
}
`
	h := reporter.NewHandler(nil)
	targetFile, err := parser.Parse("target.proto", strings.NewReader(targetRevisionSrc), h)
	require.NoError(t, err)
	plan, err := PlanImportRevisionsForAST(ctx, resolver, idx, targetFile)
	require.NoError(t, err)
	require.False(t, plan.Usable(), "notes=%v", plan.Notes)
	assert.Equal(t, ImportRevisionStatusCycle, plan.Status)
	assert.Empty(t, plan.AddImports)
	res, ok := findReference(plan, "pkga.Widget", TypeReferenceKindField)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusCycle, res.Status)
	assert.Equal(t, []string{"candidateA.proto"}, res.CandidateFiles)
}

func TestPlanImportRevisions_RecompileFailureNotUsable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The candidate defines a message whose simple name is used in a
	// parenthesized option name, which legally requires an *extension*.
	// Name- and kind-based matching cannot tell this apart (an option
	// extension reference is a message-ish qualified name), but
	// reinterpreting options during the verification recompile fails.
	// The plan must therefore be withheld rather than handing out a
	// broken import.
	srcs := map[string]string{
		"opts/opts.proto": `syntax = "proto2";

package opts;

message Thing {
  optional int32 value = 1;
}
`,
	}
	resolver := mapResolver(srcs)
	idx, err := NewSymbolFileIndex(ctx, resolver, "opts/opts.proto")
	require.NoError(t, err)

	targetSrc := `syntax = "proto2";

package consumer;

message Config {
  optional int32 x = 1 [deprecated = true];
}

// (opts.Thing) looks like an extension reference but Thing is a message,
// so this cannot link once opts/opts.proto is imported.
option (opts.Thing) = 1;
`
	unlinked := parseUnlinked(t, "consumer.proto", targetSrc)
	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	assert.False(t, plan.Usable())
	assert.Equal(t, ImportRevisionStatusRecompileFailed, plan.Status)
	assert.NotEmpty(t, plan.Notes)
}

// Three-file example from on-disk fixtures under
// internal/testdata/importrevision: a referencing file, the unique
// defining file (foo/bar.proto), and an unrelated same-short-name file
// (other/bar.proto). After the unique import is added, the descriptor
// compiles.
func TestPlanImportRevisions_ThreeFileExampleFromFixtures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := WithStandardImports(&SourceResolver{
		ImportPaths: []string{"./internal/testdata/importrevision"},
	})
	idx, err := NewSymbolFileIndex(ctx, resolver,
		"foo/bar.proto", "other/bar.proto", "public/public.proto")
	require.NoError(t, err)

	targetSrc := `syntax = "proto3";

package example.consumer;

message Example {
  foo.Bar unique = 1;
}
`
	unlinked := parseUnlinked(t, "example.proto", targetSrc)
	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	require.True(t, plan.Usable(), "notes=%v", plan.Notes)
	assert.Equal(t, []string{"foo/bar.proto"}, plan.AddImports)

	var linked linker.File
	linked, err = compileWithExtraImports(ctx, resolver, unlinked, plan.AddImports, nil)
	require.NoError(t, err)
	assert.Equal(t, "example.proto", linked.Path())
	require.NotNil(t, linked.Messages().Get(0).Fields().Get(0).Message())
	assert.Equal(t, "foo.Bar", string(linked.Messages().Get(0).Fields().Get(0).Message().FullName()))
}

func TestPlanImportRevisions_MethodTypesAndScalars(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srcs := map[string]string{
		"api/req.proto": `syntax = "proto3";

package api;

message Req {
  string q = 1;
}
`,
		"api/resp.proto": `syntax = "proto3";

package api;

message Resp {
  int32 code = 1;
}
`,
	}
	resolver := mapResolver(srcs)
	idx, err := NewSymbolFileIndex(ctx, resolver, "api/req.proto", "api/resp.proto")
	require.NoError(t, err)

	// The method input/output are real type references; the scalar field
	// types and the map key must not be mistaken for missing imports.
	targetSrc := `syntax = "proto3";

package svc;

service WidgetService {
  rpc Do (api.Req) returns (api.Resp);
}

message Local {
  string name = 1;
  map<string, int32> counts = 2;
}
`
	unlinked := parseUnlinked(t, "svc.proto", targetSrc)
	plan, err := PlanImportRevisions(ctx, resolver, idx, unlinked)
	require.NoError(t, err)
	require.True(t, plan.Usable(), "notes=%v", plan.Notes)
	assert.ElementsMatch(t, []string{"api/req.proto", "api/resp.proto"}, plan.AddImports)
	input, ok := findReference(plan, "api.Req", TypeReferenceKindMethodInput)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusUnique, input.Status)
	output, ok := findReference(plan, "api.Resp", TypeReferenceKindMethodOutput)
	require.True(t, ok)
	assert.Equal(t, ImportRevisionStatusUnique, output.Status)
	// No spurious scalar references were collected.
	for _, r := range plan.References {
		assert.NotContains(t, []string{"string", "int32"}, r.Reference.Name)
	}

	linked, err := compileWithExtraImports(ctx, resolver, unlinked, plan.AddImports, nil)
	require.NoError(t, err)
	method := linked.Services().Get(0).Methods().Get(0)
	assert.Equal(t, "api.Req", string(method.Input().FullName()))
	assert.Equal(t, "api.Resp", string(method.Output().FullName()))
}

func TestNewSymbolFileIndex_CompileFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A candidate that cannot be resolved must produce an error rather
	// than a partial index.
	resolver := mapResolver(map[string]string{
		"foo/bar.proto": importRevisionFooBarProto,
	})
	_, err := NewSymbolFileIndex(ctx, resolver, "foo/bar.proto", "missing.proto")
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}
