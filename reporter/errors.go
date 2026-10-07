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
	"fmt"

	"github.com/bufbuild/protocompile/ast"
)

// ErrInvalidSource is a sentinel error that is returned by compilation and
// stand-alone compilation steps (such as parsing, linking) when one or more
// errors is reported but the configured ErrorReporter always returns nil.
var ErrInvalidSource = errors.New("parse failed: invalid proto source")

// ErrorWithPos is an error about a proto source file that adds information
// about the location in the file that caused the error.
type ErrorWithPos interface {
	error
	ast.SourceSpan
	// GetPosition returns the start source position that caused the underlying error.
	GetPosition() ast.SourcePos
	// Unwrap returns the underlying error.
	Unwrap() error
}

// Error creates a new ErrorWithPos from the given error and source position.
func Error(span ast.SourceSpan, err error) ErrorWithPos {
	var ewp ErrorWithPos
	if errors.As(err, &ewp) {
		// replace existing position with given one
		return &errorWithSpan{SourceSpan: span, underlying: ewp.Unwrap()}
	}
	return &errorWithSpan{SourceSpan: span, underlying: err}
}

// Errorf creates a new ErrorWithPos whose underlying error is created using the
// given message format and arguments (via fmt.Errorf).
func Errorf(span ast.SourceSpan, format string, args ...any) ErrorWithPos {
	return Error(span, fmt.Errorf(format, args...))
}

// RelatedPosition describes an additional source position that is related to
// an error. For example, an error about a duplicate declaration can point at
// the conflicting (later) declaration as its primary position and include the
// location of the first declaration as a related position.
type RelatedPosition struct {
	// Span is the related location in source.
	Span ast.SourceSpan
	// Message is a short description of how this position relates to the
	// error (e.g. "first defined here").
	Message string
}

// ErrorWithRelatedPositions is an error about a proto source file that, in
// addition to the primary position provided by ErrorWithPos, exposes one or
// more additional positions that are related to the error. This allows
// diagnostics to refer to multiple locations in source, such as an error
// about a duplicate declaration that identifies the locations of both the
// first declaration and the conflicting re-declaration.
type ErrorWithRelatedPositions interface {
	ErrorWithPos
	// RelatedPositions returns the additional positions related to this
	// error, in the order in which they should be presented.
	RelatedPositions() []RelatedPosition
}

// WithRelatedPositions wraps the given error so that it also exposes the
// given related positions (see ErrorWithRelatedPositions). The returned error
// has the same primary position and message as err. If no related positions
// are given, err is returned as-is.
func WithRelatedPositions(err ErrorWithPos, related ...RelatedPosition) ErrorWithPos {
	if len(related) == 0 {
		return err
	}
	return &errorWithRelatedPositions{ErrorWithPos: err, related: related}
}

type errorWithRelatedPositions struct {
	ErrorWithPos
	related []RelatedPosition
}

func (e *errorWithRelatedPositions) RelatedPositions() []RelatedPosition {
	return e.related
}

var _ ErrorWithRelatedPositions = (*errorWithRelatedPositions)(nil)

type errorWithSpan struct {
	ast.SourceSpan
	underlying error
}

func (e *errorWithSpan) Error() string {
	sourcePos := e.GetPosition()
	return fmt.Sprintf("%s: %v", sourcePos, e.underlying)
}

func (e *errorWithSpan) GetPosition() ast.SourcePos {
	return e.Start()
}

func (e *errorWithSpan) Unwrap() error {
	return e.underlying
}

var _ ErrorWithPos = (*errorWithSpan)(nil)
