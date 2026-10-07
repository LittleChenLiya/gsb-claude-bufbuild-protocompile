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
	"slices"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/internal"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// ImportRevisionStatus describes the resolution state of a single type
// reference, and also the overall state of an [ImportRevisionPlan].
type ImportRevisionStatus int

const (
	// ImportRevisionStatusUnknown is the zero value and is never returned
	// by this package.
	ImportRevisionStatusUnknown ImportRevisionStatus = iota
	// ImportRevisionStatusResolved means the reference is already visible
	// to the file (it is defined in the file itself or reachable through
	// the file's existing imports, including transitive public imports).
	// No new import is needed.
	ImportRevisionStatusResolved
	// ImportRevisionStatusUnique means the reference matched exactly one
	// candidate file. An import is proposed for it.
	ImportRevisionStatusUnique
	// ImportRevisionStatusAmbiguous means the reference matched more than
	// one candidate file. No import is proposed because guessing between
	// the candidates could link to the wrong type.
	ImportRevisionStatusAmbiguous
	// ImportRevisionStatusUnresolved means the reference matched no
	// candidate file.
	ImportRevisionStatusUnresolved
	// ImportRevisionStatusCycle means the only matching import would
	// create an import cycle (the candidate already imports, directly or
	// transitively, the file being revised).
	ImportRevisionStatusCycle
	// ImportRevisionStatusRecompileFailed means the file, with the
	// proposed imports added, still failed to compile. The proposed plan
	// is not usable and must not be applied.
	ImportRevisionStatusRecompileFailed
	// ImportRevisionStatusUnavailable means candidate information could
	// not be established (a candidate source could not be resolved or
	// compiled, or the file to revise has no AST). No plan can be
	// produced.
	ImportRevisionStatusUnavailable
)

// String returns a human-readable description of the status.
func (s ImportRevisionStatus) String() string {
	switch s {
	case ImportRevisionStatusResolved:
		return "already resolved"
	case ImportRevisionStatusUnique:
		return "unique"
	case ImportRevisionStatusAmbiguous:
		return "ambiguous"
	case ImportRevisionStatusUnresolved:
		return "unresolved"
	case ImportRevisionStatusCycle:
		return "cycle"
	case ImportRevisionStatusRecompileFailed:
		return "recompile failed"
	case ImportRevisionStatusUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

// TypeReferenceKind is the syntactic role a type reference plays in source.
type TypeReferenceKind int

const (
	// TypeReferenceKindField is a field's (non-scalar) type, including a
	// map field's non-scalar value type.
	TypeReferenceKindField TypeReferenceKind = iota
	// TypeReferenceKindExtendee is the message extended by an "extend"
	// block.
	TypeReferenceKindExtendee
	// TypeReferenceKindMethodInput is an RPC method's request type.
	TypeReferenceKindMethodInput
	// TypeReferenceKindMethodOutput is an RPC method's response type.
	TypeReferenceKindMethodOutput
	// TypeReferenceKindExtension is a parenthesized extension name in an
	// option name, e.g. "(foo.bar)".
	TypeReferenceKindExtension
)

// String returns a human-readable description of the reference kind.
func (k TypeReferenceKind) String() string {
	switch k {
	case TypeReferenceKindField:
		return "field type"
	case TypeReferenceKindExtendee:
		return "extendee type"
	case TypeReferenceKindMethodInput:
		return "method request type"
	case TypeReferenceKindMethodOutput:
		return "method response type"
	case TypeReferenceKindExtension:
		return "extension reference"
	default:
		return "unknown"
	}
}

// TypeReference describes a single type reference extracted from an
// unlinked AST. It records the reference exactly as written in source so
// that candidate matching uses real type references rather than parsing
// compiler error text with regular expressions.
type TypeReference struct {
	// Name is the identifier as it appears in source, e.g. "foo.Bar" or
	// ".foo.Bar".
	Name string
	// Kind is the syntactic role of the reference.
	Kind TypeReferenceKind
}

// ReferenceResolution describes how a single type reference was (or could
// not be) resolved against the symbol file index.
type ReferenceResolution struct {
	// Reference is the type reference from the AST.
	Reference TypeReference
	// Status is the resolution outcome for this reference.
	Status ImportRevisionStatus
	// CandidateFiles lists import paths of candidate files:
	//   - exactly one entry when Status is Unique or Cycle;
	//   - multiple entries when Status is Ambiguous;
	//   - empty otherwise.
	CandidateFiles []string
	// Detail contains additional human-readable context.
	Detail string
}

// ImportRevisionPlan is an explicit plan for adding imports to an unlinked
// file so that its unresolved type references resolve. A plan is usable
// only when [ImportRevisionPlan.Usable] returns true, which requires that
// adding the proposed imports lets the file recompile successfully.
//
// Producing a plan never modifies the AST: its comments and declarations
// are left unchanged. All verification is performed on defensive copies.
// Ordinary [Compiler.Compile] does not perform any such revision
// automatically.
type ImportRevisionPlan struct {
	// File is the path/name of the file the plan applies to.
	File string
	// Status is the overall status of the plan. It is
	// ImportRevisionStatusUnique only for a verified, usable plan.
	Status ImportRevisionStatus
	// AddImports is the set of import paths that should be added to the
	// file. It is populated only when Status is
	// ImportRevisionStatusUnique.
	AddImports []string
	// References reports the resolution of every type reference found in
	// the file.
	References []ReferenceResolution
	// Notes contains additional diagnostic detail, such as the reason a
	// verification recompile failed.
	Notes []string
}

// Usable reports whether the plan is safe to apply. Only a unique plan
// that was verified by a successful recompile is usable.
func (p *ImportRevisionPlan) Usable() bool {
	return p != nil && p.Status == ImportRevisionStatusUnique
}

// SymbolFileIndex is an index of type symbols defined by a set of
// successfully compiled files. It is built from real descriptor data
// produced by the compiler, never by parsing error message text.
type SymbolFileIndex struct {
	// symbols maps a fully-qualified symbol name to the descriptors that
	// define it (normally at most one). Entries are limited to message,
	// enum, and service descriptors (the kinds usable as type
	// references).
	symbols map[protoreflect.FullName][]indexedSymbol
	// files maps an import path to the compiled descriptor for that file.
	files map[string]linker.File
}

// indexedSymbol pairs a type descriptor with the file that defines it.
type indexedSymbol struct {
	path string
	desc protoreflect.Descriptor
}

// kindCompatible reports whether a symbol can satisfy a reference used in
// the given role. Extendees and RPC request/response types must be
// messages; field types may be messages or enums.
func kindCompatible(descriptor protoreflect.Descriptor, usage TypeReferenceKind) bool {
	switch usage {
	case TypeReferenceKindExtendee, TypeReferenceKindMethodInput, TypeReferenceKindMethodOutput:
		_, ok := descriptor.(protoreflect.MessageDescriptor)
		return ok
	default:
		switch descriptor.(type) {
		case protoreflect.MessageDescriptor, protoreflect.EnumDescriptor:
			return true
		default:
			return false
		}
	}
}

// NewSymbolFileIndex compiles the given candidate files (using the given
// resolver to locate their source, plus all of their own imports) and
// builds an index of the type symbols they define.
//
// If any candidate file cannot be resolved or compiled, the returned
// error is non-nil and no index is returned. Candidate selection must not
// be performed against an incomplete index, since a failed candidate could
// otherwise turn an ambiguous choice into a seemingly unique one.
func NewSymbolFileIndex(ctx context.Context, resolver Resolver, candidatePaths ...string) (*SymbolFileIndex, error) {
	if resolver == nil {
		return nil, fmt.Errorf("resolver is nil")
	}
	paths := dedupStrings(candidatePaths)
	idx := &SymbolFileIndex{
		symbols: map[protoreflect.FullName][]indexedSymbol{},
		files:   map[string]linker.File{},
	}
	if len(paths) == 0 {
		return idx, nil
	}
	compiler := &Compiler{
		Resolver:       resolver,
		SourceInfoMode: SourceInfoNone,
	}
	files, err := compiler.Compile(ctx, paths...)
	if err != nil {
		return nil, fmt.Errorf("compile candidate files %v: %w", paths, err)
	}
	for _, f := range files {
		if f == nil {
			return nil, fmt.Errorf("compiler returned no descriptor for a candidate file")
		}
		idx.files[f.Path()] = f
		indexDescriptorSymbols(idx.symbols, f.Path(), f)
	}
	return idx, nil
}

func indexDescriptorSymbols(symbols map[protoreflect.FullName][]indexedSymbol, path string, fd protoreflect.FileDescriptor) {
	add := func(d protoreflect.Descriptor) {
		symbols[d.FullName()] = append(symbols[d.FullName()], indexedSymbol{path: path, desc: d})
	}
	var addMessage func(md protoreflect.MessageDescriptor)
	addMessage = func(md protoreflect.MessageDescriptor) {
		add(md)
		nested := md.Messages()
		for i := range nested.Len() {
			addMessage(nested.Get(i))
		}
		enums := md.Enums()
		for i := range enums.Len() {
			add(enums.Get(i))
		}
	}
	msgs := fd.Messages()
	for i := range msgs.Len() {
		addMessage(msgs.Get(i))
	}
	enums := fd.Enums()
	for i := range enums.Len() {
		add(enums.Get(i))
	}
	services := fd.Services()
	for i := range services.Len() {
		add(services.Get(i))
	}
}

// PlanImportRevisions produces an explicit import revision plan for the
// given unlinked parse result.
//
// The resolver is used to compile the file for verification; it must be
// able to locate the file itself, its existing imports, and any proposed
// new imports. The index provides candidate type symbols against which
// unresolved references are matched.
//
// The parse result is never modified: its descriptor proto is cloned for
// the verification recompile, and its AST is only read to discover type
// references.
func PlanImportRevisions(ctx context.Context, resolver Resolver, idx *SymbolFileIndex, result parser.Result) (*ImportRevisionPlan, error) {
	if resolver == nil {
		return nil, fmt.Errorf("resolver is nil")
	}
	if idx == nil {
		return nil, fmt.Errorf("symbol file index is nil")
	}
	if result == nil || result.FileDescriptorProto() == nil {
		return nil, fmt.Errorf("parse result is nil")
	}
	fd := result.FileDescriptorProto()
	plan := &ImportRevisionPlan{File: fd.GetName()}

	fileNode := result.AST()
	if fileNode == nil {
		plan.Status = ImportRevisionStatusUnavailable
		plan.Notes = append(plan.Notes, fmt.Sprintf("file %q has no AST; type references can only be extracted from source", plan.File))
		return plan, nil
	}

	// Compile the file as-is so references already visible through
	// existing imports (including transitive public imports) need no
	// revision. Failure here is expected (that is why we are planning);
	// it only means visibility cannot be taken for granted.
	current, _ := compileWithExtraImports(ctx, resolver, result, nil, nil)
	visible := map[protoreflect.FullName]struct{}{}
	if current != nil {
		collectVisibleSymbols(current, visible)
	}

	refs := collectTypeReferences(fileNode)

	addImports := []string{}
	added := map[string]struct{}{}
	allProposedUnique := true
	for _, ref := range refs {
		resolution := ReferenceResolution{Reference: ref}
		if current != nil && referenceIsVisible(ref.Name, string(current.Package()), visible) {
			resolution.Status = ImportRevisionStatusResolved
			plan.References = append(plan.References, resolution)
			continue
		}
		candidates := idx.matchByKind(fd.GetPackage(), ref.Name, ref.Kind)
		switch {
		case len(candidates) == 0:
			resolution.Status = ImportRevisionStatusUnresolved
			allProposedUnique = false
		case len(candidates) > 1:
			resolution.Status = ImportRevisionStatusAmbiguous
			resolution.CandidateFiles = slices.Clone(candidates)
			allProposedUnique = false
		default:
			cand := candidates[0]
			switch {
			case cand == plan.File:
				resolution.Status = ImportRevisionStatusUnresolved
				resolution.Detail = fmt.Sprintf("candidate %q is the file being revised", cand)
				allProposedUnique = false
			case createsCycle(plan.File, cand, idx):
				resolution.Status = ImportRevisionStatusCycle
				resolution.CandidateFiles = []string{cand}
				allProposedUnique = false
			default:
				resolution.Status = ImportRevisionStatusUnique
				resolution.CandidateFiles = []string{cand}
				if _, ok := added[cand]; !ok {
					added[cand] = struct{}{}
					addImports = append(addImports, cand)
				}
			}
		}
		plan.References = append(plan.References, resolution)
	}

	if !allProposedUnique {
		// Ambiguous, unresolved, or cyclic references mean no complete,
		// safe plan exists. Report the findings without proposing edits.
		plan.Status = worstReferenceStatus(plan.References)
		return plan, nil
	}

	plan.AddImports = addImports
	if len(addImports) == 0 {
		// Nothing to add: every reference was already visible.
		plan.Status = ImportRevisionStatusUnique
		return plan, nil
	}

	// Verify the plan on a defensive copy. The resolver is overlaid so
	// the modified descriptor proto is seen for the target file; no
	// source file is written or partially rewritten.
	revised, recompileErr := compileWithExtraImports(ctx, resolver, result, addImports, idx)
	if recompileErr != nil {
		plan.Status = ImportRevisionStatusRecompileFailed
		plan.Notes = append(plan.Notes, fmt.Sprintf("recompiling %q with proposed imports %v failed: %v", plan.File, addImports, recompileErr))
		return plan, nil
	}
	revisedVisible := map[protoreflect.FullName]struct{}{}
	collectVisibleSymbols(revised, revisedVisible)
	for _, ref := range refs {
		if !referenceIsVisible(ref.Name, string(revised.Package()), revisedVisible) {
			plan.Status = ImportRevisionStatusRecompileFailed
			plan.Notes = append(plan.Notes, fmt.Sprintf("reference %s (%s) still unresolved after adding proposed imports", ref.Name, ref.Kind))
			return plan, nil
		}
	}
	plan.Status = ImportRevisionStatusUnique
	return plan, nil
}

// PlanImportRevisionsForAST is a convenience wrapper that converts the
// given AST into an unlinked parse result and then calls
// [PlanImportRevisions].
func PlanImportRevisionsForAST(ctx context.Context, resolver Resolver, idx *SymbolFileIndex, file *ast.FileNode) (*ImportRevisionPlan, error) {
	if file == nil {
		return nil, fmt.Errorf("AST is nil")
	}
	result, err := parser.ResultFromAST(file, true, reporter.NewHandler(nil))
	if err != nil {
		return nil, fmt.Errorf("create unlinked descriptor proto for %q: %w", file.Name(), err)
	}
	return PlanImportRevisions(ctx, resolver, idx, result)
}

func worstReferenceStatus(refs []ReferenceResolution) ImportRevisionStatus {
	worst := ImportRevisionStatusResolved
	for _, r := range refs {
		switch r.Status {
		case ImportRevisionStatusAmbiguous:
			return ImportRevisionStatusAmbiguous
		case ImportRevisionStatusCycle:
			if worst != ImportRevisionStatusAmbiguous {
				worst = ImportRevisionStatusCycle
			}
		case ImportRevisionStatusUnresolved, ImportRevisionStatusUnavailable:
			if worst == ImportRevisionStatusResolved || worst == ImportRevisionStatusUnique {
				worst = ImportRevisionStatusUnresolved
			}
		}
	}
	if worst == ImportRevisionStatusResolved {
		// Every reference was already resolved; there is nothing to add.
		return ImportRevisionStatusUnique
	}
	return worst
}

// matchByKind returns the distinct, sorted set of candidate files that
// define a symbol the given reference could legally resolve to from a
// file with the given package, restricted to symbols whose descriptor
// kind is valid for the reference's role.
//
// It first applies protobuf scoping rules exactly as protoc does: an
// unqualified reference is tried against the file's package and each
// enclosing parent package, and a leading dot denotes a fully qualified
// name. If that yields no match, it falls back to matching by the
// reference's simple (last) name, which is the behavior expected of an
// import-assist feature when the reference uses a name relative to the
// package that declares the type (rather than the revising file's
// package). All matching files are returned so ambiguity can be reported
// instead of guessed.
func (idx *SymbolFileIndex) matchByKind(pkg, ref string, kind TypeReferenceKind) []string {
	pathSet := map[string]struct{}{}
	addSymbols := func(syms []indexedSymbol) {
		for _, s := range syms {
			if !kindCompatible(s.desc, kind) {
				continue
			}
			pathSet[s.path] = struct{}{}
		}
	}
	for _, name := range scopedTrialNames(pkg, ref) {
		addSymbols(idx.symbols[name])
	}
	if len(pathSet) == 0 {
		simple := simpleName(ref)
		names := make([]protoreflect.FullName, 0, len(idx.symbols))
		for name := range idx.symbols {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
		for _, name := range names {
			if simpleName(string(name)) == simple {
				addSymbols(idx.symbols[name])
			}
		}
	}
	result := make([]string, 0, len(pathSet))
	for p := range pathSet {
		result = append(result, p)
	}
	sort.Strings(result)
	return result
}

// scopedTrialNames computes the fully-qualified names protoc would try
// for a reference used in a file with the given package.
func scopedTrialNames(pkg, ref string) []protoreflect.FullName {
	if strings.HasPrefix(ref, ".") {
		return []protoreflect.FullName{protoreflect.FullName(strings.TrimPrefix(ref, "."))}
	}
	prefixes := internal.CreatePrefixList(pkg)
	names := make([]protoreflect.FullName, 0, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			names = append(names, protoreflect.FullName(ref))
		} else {
			names = append(names, protoreflect.FullName(prefix+"."+ref))
		}
	}
	return names
}

func simpleName(name string) string {
	name = strings.TrimPrefix(name, ".")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// createsCycle reports whether adding "import candidate" to targetFile
// would create a cycle: candidate directly or transitively imports
// targetFile.
func createsCycle(targetFile, candidate string, idx *SymbolFileIndex) bool {
	candFile := idx.files[candidate]
	if candFile == nil {
		return false
	}
	visited := map[string]struct{}{}
	var walk func(path string) bool
	walk = func(path string) bool {
		if path == targetFile {
			return true
		}
		if _, ok := visited[path]; ok {
			return false
		}
		visited[path] = struct{}{}
		f := idx.files[path]
		if f == nil {
			// Not part of the indexed candidate set; its imports are not
			// visible to us and cannot establish a cycle.
			return false
		}
		imports := f.Imports()
		for i := range imports.Len() {
			if walk(imports.Get(i).Path()) {
				return true
			}
		}
		return false
	}
	imports := candFile.Imports()
	for i := range imports.Len() {
		if walk(imports.Get(i).Path()) {
			return true
		}
	}
	return false
}

// compileWithExtraImports clones the given parse result (so its
// descriptor proto and node index are defensively copied), appends the
// extra imports to the clone's dependency list, and compiles the clone.
// The original parse result is never modified; no source is rewritten.
//
// When candidateIndex is non-nil, indexed files are supplied as linked
// descriptors through an overlay resolver; other paths are delegated to
// the caller's resolver. This lets verification link the target even
// when the resolver's own copy of an indexed file differs from the one
// indexed.
//
// A nil file with a non-nil error indicates the file failed to compile.
func compileWithExtraImports(ctx context.Context, resolver Resolver, result parser.Result, extraImports []string, candidateIndex *SymbolFileIndex) (linker.File, error) {
	clone := parser.Clone(result)
	fd := clone.FileDescriptorProto()
	fd.Dependency = dedupStrings(append(slices.Clone(fd.Dependency), extraImports...))

	effective := ResolverFunc(func(path string) (SearchResult, error) {
		if path == fd.GetName() {
			return SearchResult{ParseResult: clone}, nil
		}
		if candidateIndex != nil {
			if f, ok := candidateIndex.files[path]; ok {
				return SearchResult{Desc: f}, nil
			}
		}
		return resolver.FindFileByPath(path)
	})
	compiler := &Compiler{
		Resolver:       effective,
		SourceInfoMode: SourceInfoNone,
	}
	files, err := compiler.Compile(ctx, fd.GetName())
	if err != nil {
		return nil, err
	}
	if len(files) == 0 || files[0] == nil {
		return nil, fmt.Errorf("no result compiling %q", fd.GetName())
	}
	return files[0], nil
}

// collectVisibleSymbols populates visible with the fully-qualified names
// of all type descriptors visible to the file: those defined in the file
// itself, in its direct imports, or reachable through chains of public
// imports (matching the linker's transitive visibility rules).
func collectVisibleSymbols(f protoreflect.FileDescriptor, visible map[protoreflect.FullName]struct{}) {
	seenFiles := map[string]struct{}{}
	var addFile func(fd protoreflect.FileDescriptor, publicOnly bool)
	addFile = func(fd protoreflect.FileDescriptor, publicOnly bool) {
		if fd == nil {
			return
		}
		if _, ok := seenFiles[fd.Path()]; ok {
			return
		}
		seenFiles[fd.Path()] = struct{}{}
		indexVisibleSymbols(visible, fd)
		imports := fd.Imports()
		for i := range imports.Len() {
			imp := imports.Get(i)
			if publicOnly && !imp.IsPublic {
				continue
			}
			addFile(imp.FileDescriptor, true)
		}
	}
	addFile(f, false)
}

func indexVisibleSymbols(symbols map[protoreflect.FullName]struct{}, fd protoreflect.FileDescriptor) {
	var addMessage func(md protoreflect.MessageDescriptor)
	addMessage = func(md protoreflect.MessageDescriptor) {
		symbols[md.FullName()] = struct{}{}
		nested := md.Messages()
		for i := range nested.Len() {
			addMessage(nested.Get(i))
		}
		enums := md.Enums()
		for i := range enums.Len() {
			symbols[enums.Get(i).FullName()] = struct{}{}
		}
	}
	msgs := fd.Messages()
	for i := range msgs.Len() {
		addMessage(msgs.Get(i))
	}
	enums := fd.Enums()
	for i := range enums.Len() {
		symbols[enums.Get(i).FullName()] = struct{}{}
	}
	services := fd.Services()
	for i := range services.Len() {
		symbols[services.Get(i).FullName()] = struct{}{}
	}
}

// referenceIsVisible reports whether the reference, interpreted using
// protobuf scoping rules for the given package, names a symbol already
// visible to the file.
func referenceIsVisible(ref, pkg string, visible map[protoreflect.FullName]struct{}) bool {
	for _, name := range scopedTrialNames(pkg, ref) {
		if _, ok := visible[name]; ok {
			return true
		}
	}
	return false
}

// scalarFieldTypes is the set of built-in scalar field type names. These
// references never require an import.
var scalarFieldTypes = map[string]struct{}{
	"double": {}, "float": {}, "int32": {}, "int64": {},
	"uint32": {}, "uint64": {}, "sint32": {}, "sint64": {},
	"fixed32": {}, "fixed64": {}, "sfixed32": {}, "sfixed64": {},
	"bool": {}, "string": {}, "bytes": {},
}

// collectTypeReferences extracts real type references from an unlinked
// AST. It does not look at any compiler error message.
func collectTypeReferences(file *ast.FileNode) []TypeReference {
	var refs []TypeReference
	addType := func(node ast.IdentValueNode, kind TypeReferenceKind) {
		if node == nil {
			return
		}
		name := string(node.AsIdentifier())
		if kind == TypeReferenceKindField || kind == TypeReferenceKindExtendee {
			if _, ok := scalarFieldTypes[simpleName(name)]; ok {
				return
			}
		}
		refs = append(refs, TypeReference{Name: name, Kind: kind})
	}
	var walkMessageElements func(decls []ast.MessageElement)
	var walkEnum func(node *ast.EnumNode)
	var walkFieldOptions func(options *ast.CompactOptionsNode)
	var walkOption func(opt *ast.OptionNode)
	var walkMessage func(node *ast.MessageNode)

	walkOptionName := func(name *ast.OptionNameNode) {
		if name == nil {
			return
		}
		for _, part := range name.Parts {
			if part.Open != nil {
				// Parenthesized name: an extension reference.
				addType(part.Name, TypeReferenceKindExtension)
			}
		}
	}
	walkOption = func(opt *ast.OptionNode) {
		if opt == nil {
			return
		}
		walkOptionName(opt.Name)
	}
	walkFieldOptions = func(options *ast.CompactOptionsNode) {
		if options == nil {
			return
		}
		for _, opt := range options.Options {
			walkOption(opt)
		}
	}
	walkField := func(node ast.FieldDeclNode) {
		// FieldType for a *MapFieldNode is the *MapTypeNode; for a
		// *GroupNode it is the group keyword (a type declared inline in
		// the same file), which we skip.
		switch fld := node.(type) {
		case *ast.MapFieldNode:
			addType(fld.MapType.ValueType, TypeReferenceKindField)
		case *ast.FieldNode:
			addType(fld.FldType, TypeReferenceKindField)
		case *ast.GroupNode:
			// inline declaration; nothing to import
		}
		if opts := node.GetOptions(); opts != nil {
			walkFieldOptions(opts)
		}
	}
	walkExtend := func(node *ast.ExtendNode) {
		if node == nil {
			return
		}
		addType(node.Extendee, TypeReferenceKindExtendee)
		for _, decl := range node.Decls {
			switch decl := decl.(type) {
			case *ast.FieldNode:
				walkField(decl)
			case *ast.GroupNode:
				walkField(decl)
			}
		}
	}
	walkEnum = func(node *ast.EnumNode) {
		if node == nil {
			return
		}
		for _, decl := range node.Decls {
			if opt, ok := decl.(*ast.OptionNode); ok {
				walkOption(opt)
			}
		}
	}
	walkMessage = func(node *ast.MessageNode) {
		if node == nil {
			return
		}
		for _, decl := range node.Decls {
			if opt, ok := decl.(*ast.OptionNode); ok {
				walkOption(opt)
			}
		}
		walkMessageElements(node.Decls)
	}
	walkMessageElements = func(decls []ast.MessageElement) {
		for _, decl := range decls {
			switch decl := decl.(type) {
			case *ast.FieldNode:
				walkField(decl)
			case *ast.MapFieldNode:
				walkField(decl)
			case *ast.GroupNode:
				walkField(decl)
				// A group declares an inline message whose body must
				// also be walked.
				walkMessageElements(decl.Decls)
			case *ast.MessageNode:
				walkMessage(decl)
			case *ast.EnumNode:
				walkEnum(decl)
			case *ast.ExtendNode:
				walkExtend(decl)
			case *ast.OneofNode:
				for _, oneofDecl := range decl.Decls {
					switch d := oneofDecl.(type) {
					case *ast.FieldNode:
						walkField(d)
					case *ast.GroupNode:
						walkField(d)
					case *ast.OptionNode:
						walkOption(d)
					}
				}
			case *ast.OptionNode:
				walkOption(decl)
			}
		}
	}
	walkService := func(node *ast.ServiceNode) {
		if node == nil {
			return
		}
		for _, decl := range node.Decls {
			rpc, ok := decl.(*ast.RPCNode)
			if !ok {
				if opt, ok := decl.(*ast.OptionNode); ok {
					walkOption(opt)
				}
				continue
			}
			if input := rpc.Input; input != nil {
				addType(input.MessageType, TypeReferenceKindMethodInput)
			}
			if output := rpc.Output; output != nil {
				addType(output.MessageType, TypeReferenceKindMethodOutput)
			}
			for _, rpcDecl := range rpc.Decls {
				if opt, ok := rpcDecl.(*ast.OptionNode); ok {
					walkOption(opt)
				}
			}
		}
	}

	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.MessageNode:
			walkMessage(decl)
		case *ast.EnumNode:
			walkEnum(decl)
		case *ast.ExtendNode:
			walkExtend(decl)
		case *ast.ServiceNode:
			walkService(decl)
		case *ast.OptionNode:
			walkOption(decl)
		}
	}
	return refs
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := map[string]struct{}{}
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
