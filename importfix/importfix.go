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

// Package importfix computes explicit import revision plans for protobuf
// source files whose ASTs cannot be linked because of missing imports.
//
// The planner inspects the real type references of a parsed (but unlinked)
// file -- field types, extendees, and RPC input/output types as recorded in
// the file's descriptor proto -- and resolves them against a caller-supplied
// SymbolIndex that maps fully-qualified symbols to the files that define
// them. It never guesses names by pattern-matching compiler error text.
//
// A new import is only suggested when exactly one candidate file can provide
// an otherwise unresolvable symbol. If several files could provide the
// symbol, or every candidate would introduce an import cycle, the symbol is
// reported as unresolved instead. Symbols that are already visible through
// existing (possibly public) imports are left alone, so existing public
// import paths are always used before new imports are considered.
//
// A plan is only marked usable (Plan.Verified) when the revised source --
// the original bytes with only new import lines inserted, never any edits to
// existing comments or declarations -- has been re-compiled successfully.
// If verification fails or some symbols remain unresolved, no revised source
// is emitted at all, so callers never receive a half-fixed file.
//
// Note that the normal protocompile.Compiler.Compile operation never applies
// these fixes automatically; producing and applying a plan is always an
// explicit, opt-in step through this package.
package importfix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// Planner computes import revision plans for files with missing imports.
type Planner struct {
	// Resolver provides source (or other representations) for the files that
	// are analyzed as well as their transitive dependencies. It is used to
	// compute symbol visibility, to detect import cycles, and to re-compile
	// revised sources for verification. This field is required.
	Resolver protocompile.Resolver
	// Index maps fully-qualified symbol names to the files that define
	// them. It is the source of candidate imports for symbols that are not
	// already visible. This field is required.
	Index *SymbolIndex
}

// Plan is the result of analyzing a single file.
type Plan struct {
	// Path is the path of the analyzed file, as passed to the Planner.
	Path string
	// ImportsToAdd lists the new import paths, in the order they would be
	// inserted into the file. It is empty if no new imports are needed or
	// if no unambiguous candidates could be determined.
	ImportsToAdd []string
	// Unresolved lists the symbols for which no safe import could be
	// suggested. If it is non-empty, the plan is not directly usable.
	Unresolved []Unresolved
	// Verified reports whether the revised source was re-compiled
	// successfully. It is also true when no changes were necessary at all.
	Verified bool
	// RevisedSource is the complete revised contents of the file: the
	// original bytes with only new import lines inserted. It is only set
	// when Verified is true and at least one import was added; it is nil
	// otherwise, so a caller never receives a half-fixed file.
	RevisedSource []byte
	// VerifyErr is the error encountered when re-compiling the revised
	// source, if verification was attempted and failed.
	VerifyErr error
}

// Usable reports whether the plan can be applied as-is: every reference
// resolved and (if changes were needed) the revised source compiled.
func (p *Plan) Usable() bool {
	return p != nil && p.Verified && len(p.Unresolved) == 0
}

// Reason describes why a symbol could not be resolved to a new import.
type Reason int

const (
	// ReasonNotFound indicates that the symbol index contains no file that
	// defines the symbol.
	ReasonNotFound Reason = iota
	// ReasonAmbiguous indicates that more than one file could provide the
	// symbol, so no single import can be chosen safely.
	ReasonAmbiguous
	// ReasonCycle indicates that every candidate file (transitively)
	// imports the file being fixed, so importing it would create a cycle.
	ReasonCycle
)

func (r Reason) String() string {
	switch r {
	case ReasonNotFound:
		return "not found"
	case ReasonAmbiguous:
		return "ambiguous"
	case ReasonCycle:
		return "cycle risk"
	default:
		return fmt.Sprintf("unknown(%d)", int(r))
	}
}

// Unresolved describes a single symbol that could not be mapped to a
// unique, safe new import.
type Unresolved struct {
	// Symbol is the fully-qualified name that could not be resolved.
	Symbol string
	// Reason explains why no import was suggested.
	Reason Reason
	// Candidates lists the files that were considered, if any. For
	// ReasonAmbiguous these are the files that all define the symbol; for
	// ReasonCycle these are the files that would create an import cycle.
	Candidates []string
}

// Plan analyzes the file at the given path and computes an import revision
// plan for it. The resolver must be able to provide source for the file
// itself; other representations are accepted for its dependencies.
func (p *Planner) Plan(ctx context.Context, path string) (*Plan, error) {
	res, err := p.Resolver.FindFileByPath(path)
	if err != nil {
		return nil, fmt.Errorf("importfix: could not resolve %q: %w", path, err)
	}
	if res.Source == nil {
		return nil, fmt.Errorf("importfix: resolver must provide source for %q to compute a plan", path)
	}
	src, err := io.ReadAll(res.Source)
	if err != nil {
		return nil, fmt.Errorf("importfix: could not read source for %q: %w", path, err)
	}
	handler := reporter.NewHandler(nil)
	file, err := parser.Parse(path, bytes.NewReader(src), handler)
	if err != nil {
		return nil, fmt.Errorf("importfix: could not parse %q: %w", path, err)
	}
	return p.PlanAST(ctx, path, src, file)
}

// PlanAST is like Plan but operates on an already-parsed (yet unlinked) AST
// and its original source bytes.
func (p *Planner) PlanAST(ctx context.Context, path string, src []byte, file *ast.FileNode) (*Plan, error) {
	if p.Resolver == nil {
		return nil, errors.New("importfix: Planner.Resolver is required")
	}
	if p.Index == nil {
		return nil, errors.New("importfix: Planner.Index is required")
	}
	handler := reporter.NewHandler(nil)
	result, err := parser.ResultFromAST(file, false, handler)
	if err != nil {
		return nil, fmt.Errorf("importfix: could not interpret AST for %q: %w", path, err)
	}
	return p.plan(ctx, path, src, file, result.FileDescriptorProto())
}
