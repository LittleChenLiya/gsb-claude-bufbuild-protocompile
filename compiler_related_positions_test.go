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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"

	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
)

// In-memory fixtures for duplicate declaration tests.
const (
	// relatedPosDupFile declares pkg.Foo twice in the same file: first at
	// line 4, column 9 and again at line 6, column 9.
	relatedPosDupFile = `syntax = "proto3";
package pkg;

message Foo {}
message Bar {}
message Foo {}
`
	// relatedPosAFile declares pkg.Foo at line 4, column 9.
	relatedPosAFile = `syntax = "proto3";
package pkg;

message Foo {}
`
	// relatedPosBFile imports a.proto and re-declares pkg.Foo at line 6,
	// column 9.
	relatedPosBFile = `syntax = "proto3";
package pkg;

import "a.proto";

message Foo {}
`
	// relatedPosXFile and relatedPosYFile legally declare the same symbol
	// name in two different packages.
	relatedPosXFile = `syntax = "proto3";
package p1;

message Foo {}
`
	relatedPosYFile = `syntax = "proto3";
package p2;

message Foo {}
`
)

// compileWithRelatedPositions compiles the given files from the given
// in-memory sources and returns the linked files and all reported errors.
func compileWithRelatedPositions(t *testing.T, includeRelatedPositions bool, srcs map[string]string, files ...string) (linker.Files, []reporter.ErrorWithPos) {
	t.Helper()
	var reported []reporter.ErrorWithPos
	compiler := Compiler{
		Resolver: &SourceResolver{
			Accessor: SourceAccessorFromMap(srcs),
		},
		Symbols: &linker.Symbols{
			IncludeRelatedPositions: includeRelatedPositions,
		},
		Reporter: reporter.NewReporter(func(err reporter.ErrorWithPos) error {
			reported = append(reported, err)
			return nil
		}, nil),
	}
	linked, err := compiler.Compile(context.Background(), files...)
	if len(reported) > 0 {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
	}
	return linked, reported
}

func requireRelatedPosition(t *testing.T, err reporter.ErrorWithPos, wantFile string, wantLine, wantCol int) {
	t.Helper()
	withRelated, ok := err.(reporter.ErrorWithRelatedPositions)
	require.True(t, ok, "error does not implement reporter.ErrorWithRelatedPositions: %v", err)
	related := withRelated.RelatedPositions()
	require.Len(t, related, 1)
	assert.Equal(t, "first defined here", related[0].Message)
	pos := related[0].Span.Start()
	assert.Equal(t, wantFile, pos.Filename)
	assert.Equal(t, wantLine, pos.Line)
	assert.Equal(t, wantCol, pos.Col)
}

func requirePrimaryPosition(t *testing.T, err reporter.ErrorWithPos, wantFile string, wantLine, wantCol int) {
	t.Helper()
	pos := err.GetPosition()
	assert.Equal(t, wantFile, pos.Filename)
	assert.Equal(t, wantLine, pos.Line)
	assert.Equal(t, wantCol, pos.Col)
}

// Same-file duplicate declaration: the primary error position is the later
// declaration and the first declaration is attached as a related position.
func TestRelatedPositions_SameFileConflict(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{"dup.proto": relatedPosDupFile}
	_, reported := compileWithRelatedPositions(t, true, srcs, "dup.proto")

	require.Len(t, reported, 1)
	err := reported[0]
	assert.Equal(t, `dup.proto:6:9: symbol "pkg.Foo" already defined`, err.Error())
	// primary position is the later (second) declaration
	requirePrimaryPosition(t, err, "dup.proto", 6, 9)
	// related position is the first declaration
	requireRelatedPosition(t, err, "dup.proto", 4, 9)
}

// Cross-file conflict through an import: b.proto imports a.proto and
// re-declares pkg.Foo. The conflict is located in both files.
func TestRelatedPositions_CrossFileConflict(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{
		"a.proto": relatedPosAFile,
		"b.proto": relatedPosBFile,
	}
	_, reported := compileWithRelatedPositions(t, true, srcs, "b.proto")

	require.Len(t, reported, 1)
	err := reported[0]
	assert.Equal(t, `b.proto:6:9: symbol "pkg.Foo" already defined`, err.Error())
	// primary position is the conflicting declaration in b.proto
	requirePrimaryPosition(t, err, "b.proto", 6, 9)
	// related position is the first declaration, in the imported a.proto
	requireRelatedPosition(t, err, "a.proto", 4, 9)
}

// Cross-file conflict where both files are compiled together: exactly one
// error is reported (for the file that linked second), and its two positions
// span both files.
func TestRelatedPositions_CrossFileConflictBothRoots(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{
		"a.proto": relatedPosAFile,
		"b.proto": `syntax = "proto3";
package pkg;

message Foo {}
`,
	}
	_, reported := compileWithRelatedPositions(t, true, srcs, "a.proto", "b.proto")

	require.Len(t, reported, 1)
	err := reported[0]
	primary := err.GetPosition()
	withRelated, ok := err.(reporter.ErrorWithRelatedPositions)
	require.True(t, ok)
	related := withRelated.RelatedPositions()
	require.Len(t, related, 1)
	other := related[0].Span.Start()
	// the two positions are in different files: the primary position is in
	// whichever file linked last, the related position in the other one
	assert.NotEqual(t, primary.Filename, other.Filename)
	assert.Equal(t, 4, primary.Line)
	assert.Equal(t, 9, primary.Col)
	assert.Equal(t, 4, other.Line)
	assert.Equal(t, 9, other.Col)
}

// Legal case: the same symbol name in two different packages is not a
// conflict, and enabling the option does not change the linked result.
func TestRelatedPositions_DifferentPackagesLinkSuccessfully(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{
		"x.proto": relatedPosXFile,
		"y.proto": relatedPosYFile,
	}

	linkedWithOption, reported := compileWithRelatedPositions(t, true, srcs, "x.proto", "y.proto")
	require.Empty(t, reported)
	require.Len(t, linkedWithOption, 2)

	// The successful link output is identical whether or not the option is
	// enabled.
	linkedWithoutOption, reported := compileWithRelatedPositions(t, false, srcs, "x.proto", "y.proto")
	require.Empty(t, reported)
	require.Len(t, linkedWithoutOption, 2)

	for i := range linkedWithOption {
		withOpt := protodesc.ToFileDescriptorProto(linkedWithOption[i])
		withoutOpt := protodesc.ToFileDescriptorProto(linkedWithoutOption[i])
		require.True(t, proto.Equal(withOpt, withoutOpt),
			"linked output for %s differs with option enabled", linkedWithOption[i].Path())
	}
}

// When the option is not enabled, errors are reported exactly as before: the
// first declaration's position appears only in the message text and the error
// does not implement reporter.ErrorWithRelatedPositions.
func TestRelatedPositions_OptionDisabledKeepsOriginalBehavior(t *testing.T) {
	t.Parallel()
	srcs := map[string]string{"dup.proto": relatedPosDupFile}
	_, reported := compileWithRelatedPositions(t, false, srcs, "dup.proto")

	require.Len(t, reported, 1)
	err := reported[0]
	// same message format as before this option existed
	assert.Equal(t, `dup.proto:6:9: symbol "pkg.Foo" already defined at dup.proto:4:9`, err.Error())
	requirePrimaryPosition(t, err, "dup.proto", 6, 9)
	_, ok := err.(reporter.ErrorWithRelatedPositions)
	assert.False(t, ok, "error should not expose related positions when option is disabled")
}

// ExampleCompiler_includeRelatedPositions demonstrates the dual-position
// diagnostic reported for a duplicate declaration when the
// linker.Symbols.IncludeRelatedPositions option is enabled: the primary
// error position is the later declaration, and the location of the first
// declaration (possibly in another, imported file) is attached as a related
// position.
func ExampleCompiler_includeRelatedPositions() {
	var reported []reporter.ErrorWithPos
	compiler := Compiler{
		Resolver: &SourceResolver{
			Accessor: SourceAccessorFromMap(map[string]string{
				"a.proto": relatedPosAFile,
				"b.proto": relatedPosBFile,
			}),
		},
		Symbols: &linker.Symbols{IncludeRelatedPositions: true},
		Reporter: reporter.NewReporter(func(err reporter.ErrorWithPos) error {
			reported = append(reported, err)
			return nil
		}, nil),
	}
	_, _ = compiler.Compile(context.Background(), "b.proto")
	for _, err := range reported {
		fmt.Println(err)
		if withRelated, ok := err.(reporter.ErrorWithRelatedPositions); ok {
			for _, related := range withRelated.RelatedPositions() {
				fmt.Printf("  %v: %s\n", related.Span.Start(), related.Message)
			}
		}
	}

	// Output:
	// b.proto:6:9: symbol "pkg.Foo" already defined
	//   a.proto:4:9: first defined here
}
