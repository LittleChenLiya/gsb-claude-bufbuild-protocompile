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

package reporter

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bufbuild/protocompile/ast"
)

func TestWithRelatedPositions(t *testing.T) {
	t.Parallel()

	primarySpan := ast.NewSourceSpan(
		ast.SourcePos{Filename: "b.proto", Line: 6, Col: 9},
		ast.SourcePos{Filename: "b.proto", Line: 6, Col: 12},
	)
	firstDeclSpan := ast.NewSourceSpan(
		ast.SourcePos{Filename: "a.proto", Line: 4, Col: 9},
		ast.SourcePos{Filename: "a.proto", Line: 4, Col: 12},
	)

	base := Errorf(primarySpan, "symbol %q already defined", "pkg.Foo")

	// With no related positions, the error is returned as-is.
	require.Same(t, base, WithRelatedPositions(base))
	// And it does not implement ErrorWithRelatedPositions.
	_, ok := base.(ErrorWithRelatedPositions)
	require.False(t, ok)

	err := WithRelatedPositions(base, RelatedPosition{
		Span:    firstDeclSpan,
		Message: "first defined here",
	})

	// The wrapped error keeps the same primary position and message.
	require.Equal(t, base.Error(), err.Error())
	require.Equal(t, base.GetPosition(), err.GetPosition())
	require.Equal(t, base.Start(), err.Start())
	require.Equal(t, base.End(), err.End())

	// The related positions are exposed via ErrorWithRelatedPositions.
	withRelated, ok := err.(ErrorWithRelatedPositions)
	require.True(t, ok)
	related := withRelated.RelatedPositions()
	require.Len(t, related, 1)
	assert.Equal(t, firstDeclSpan.Start(), related[0].Span.Start())
	assert.Equal(t, firstDeclSpan.End(), related[0].Span.End())
	assert.Equal(t, "first defined here", related[0].Message)

	// The underlying error is still unwrapped the same way.
	require.Equal(t, base.Unwrap(), errors.Unwrap(err))
}
