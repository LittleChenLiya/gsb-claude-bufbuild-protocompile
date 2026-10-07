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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/reporter"
)

// errorCollector is a reporter that collects all reported errors so tests can
// examine their count, positions, and related spans.
type errorCollector struct {
	errs []reporter.ErrorWithPos
}

func (c *errorCollector) reporter() reporter.Reporter {
	return reporter.NewReporter(func(err reporter.ErrorWithPos) error {
		c.errs = append(c.errs, err)
		return nil
	}, nil)
}

func assertSourcePos(t *testing.T, expectedFile string, expectedLine, expectedCol int, actual ast.SourcePos) {
	t.Helper()
	assert.Equal(t, expectedFile, actual.Filename)
	assert.Equal(t, expectedLine, actual.Line)
	assert.Equal(t, expectedCol, actual.Col)
}

func requireRelatedSpans(t *testing.T, err reporter.ErrorWithPos) []reporter.RelatedSpan {
	t.Helper()
	withRelated, ok := err.(reporter.ErrorWithRelatedSpans)
	require.True(t, ok, "error %T does not implement reporter.ErrorWithRelatedSpans", err)
	related := withRelated.RelatedSpans()
	require.Len(t, related, 1)
	return related
}

func TestDuplicateSymbolLocations(t *testing.T) {
	t.Parallel()

	t.Run("same_file", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{
			"dup.proto": `syntax = "proto3";
package p;

message M {}
message M {}
`,
		}
		collector := &errorCollector{}
		compiler := Compiler{
			Resolver: &SourceResolver{
				Accessor: SourceAccessorFromMap(files),
			},
			Reporter:                       collector.reporter(),
			ReportDuplicateSymbolLocations: true,
		}
		_, err := compiler.Compile(t.Context(), "dup.proto")
		require.Error(t, err)

		// exactly one error, positioned at the later (second) declaration
		require.Len(t, collector.errs, 1)
		repErr := collector.errs[0]
		assertSourcePos(t, "dup.proto", 5, 9, repErr.GetPosition())
		// first declaration attached as a related span
		related := requireRelatedSpans(t, repErr)
		assertSourcePos(t, "dup.proto", 4, 9, related[0].Span.Start())
		t.Logf("same-file diagnostic: %v", repErr)
	})

	t.Run("cross_file", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{
			"a.proto": `syntax = "proto3";
package p;

message M {}
`,
			"b.proto": `syntax = "proto3";
package p;

import "a.proto";

message M {}
`,
		}
		collector := &errorCollector{}
		compiler := Compiler{
			Resolver: &SourceResolver{
				Accessor: SourceAccessorFromMap(files),
			},
			Reporter:                       collector.reporter(),
			ReportDuplicateSymbolLocations: true,
		}
		_, err := compiler.Compile(t.Context(), "b.proto")
		require.Error(t, err)

		// exactly one error, positioned at the later declaration in b.proto
		require.Len(t, collector.errs, 1)
		repErr := collector.errs[0]
		assertSourcePos(t, "b.proto", 6, 9, repErr.GetPosition())
		// first declaration is in the imported file a.proto
		related := requireRelatedSpans(t, repErr)
		assertSourcePos(t, "a.proto", 4, 9, related[0].Span.Start())
		t.Logf("cross-file diagnostic: %v", repErr)
	})

	t.Run("cross_file_via_linker_symbols", func(t *testing.T) {
		t.Parallel()
		// same scenario, but the option is supplied by explicitly creating
		// the symbol table with linker.NewSymbols
		files := map[string]string{
			"a.proto": `syntax = "proto3";
package p;

message M {}
`,
			"b.proto": `syntax = "proto3";
package p;

import "a.proto";

message M {}
`,
		}
		collector := &errorCollector{}
		compiler := Compiler{
			Resolver: &SourceResolver{
				Accessor: SourceAccessorFromMap(files),
			},
			Reporter: collector.reporter(),
			Symbols:  linker.NewSymbols(linker.WithDuplicateSymbolLocations()),
		}
		_, err := compiler.Compile(t.Context(), "b.proto")
		require.Error(t, err)

		require.Len(t, collector.errs, 1)
		repErr := collector.errs[0]
		assertSourcePos(t, "b.proto", 6, 9, repErr.GetPosition())
		related := requireRelatedSpans(t, repErr)
		assertSourcePos(t, "a.proto", 4, 9, related[0].Span.Start())
		t.Logf("cross-file diagnostic (explicit symbols): %v", repErr)
	})

	t.Run("distinct_packages_ok", func(t *testing.T) {
		t.Parallel()
		// same symbol name in different packages is legal and must link
		// successfully, with identical output whether or not the option is on
		files := map[string]string{
			"a.proto": `syntax = "proto3";
package p1;

message M {}
`,
			"b.proto": `syntax = "proto3";
package p2;

import "a.proto";

message M {
  p1.M other = 1;
}
`,
		}
		compile := func(reportLocations bool) (linker.Files, error) {
			compiler := Compiler{
				Resolver: &SourceResolver{
					Accessor: SourceAccessorFromMap(files),
				},
				ReportDuplicateSymbolLocations: reportLocations,
			}
			return compiler.Compile(t.Context(), "b.proto")
		}
		withOpt, err := compile(true)
		require.NoError(t, err)
		require.Len(t, withOpt, 1)
		withoutOpt, err := compile(false)
		require.NoError(t, err)
		require.Len(t, withoutOpt, 1)

		// the linked result is the same with the option on or off
		require.Equal(t, "p2.M", string(withOpt[0].Messages().Get(0).FullName()))
		require.True(t, proto.Equal(
			protodesc.ToFileDescriptorProto(withOpt[0]),
			protodesc.ToFileDescriptorProto(withoutOpt[0]),
		), "linked output differs when option is enabled")
		t.Logf("linked successfully: %s (output identical with option on/off)", withOpt[0].Messages().Get(0).FullName())
	})

	t.Run("option_disabled_keeps_legacy_behavior", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{
			"dup.proto": `syntax = "proto3";
package p;

message M {}
message M {}
`,
		}
		collector := &errorCollector{}
		compiler := Compiler{
			Resolver: &SourceResolver{
				Accessor: SourceAccessorFromMap(files),
			},
			Reporter: collector.reporter(),
			// ReportDuplicateSymbolLocations left false
		}
		_, err := compiler.Compile(t.Context(), "dup.proto")
		require.Error(t, err)

		require.Len(t, collector.errs, 1)
		repErr := collector.errs[0]
		// legacy single-position error: no related spans, position in text
		_, ok := repErr.(reporter.ErrorWithRelatedSpans)
		assert.False(t, ok, "legacy error should not implement reporter.ErrorWithRelatedSpans")
		assertSourcePos(t, "dup.proto", 5, 9, repErr.GetPosition())
		assert.Equal(t, `dup.proto:5:9: symbol "p.M" already defined at dup.proto:4:9`, repErr.Error())
		t.Logf("legacy diagnostic: %v", repErr)
	})
}
