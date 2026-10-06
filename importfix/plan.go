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
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/bufbuild/protocompile/reporter"
)

// importRef is a single import declaration in a file.
type importRef struct {
	path   string
	public bool
}

// fileFacts holds the facts about a file that planning needs: its imports
// and the set of fully-qualified symbols it defines.
type fileFacts struct {
	imports []importRef
	symbols map[string]struct{}
}

// typeRef is a real type reference found in a file, with the fully-qualified
// scope (without leading dot) in which the reference appears.
type typeRef struct {
	scope string
	name  string
}

// planRunner carries per-planning-operation state.
type planRunner struct {
	planner *Planner
	cache   map[string]*fileFacts // nil entry means the file could not be loaded
}

func (p *Planner) plan(ctx context.Context, path string, src []byte, file *ast.FileNode, fdp *descriptorpb.FileDescriptorProto) (*Plan, error) {
	target := factsFromProto(fdp)
	r := &planRunner{
		planner: p,
		cache:   map[string]*fileFacts{path: target},
	}

	// Compute the set of files whose symbols are visible to the target:
	// its direct imports plus anything reachable via public imports. This
	// is how existing public import paths get used first: a symbol that is
	// already re-exported through one never triggers a new import.
	visibleFiles := map[string]bool{path: true}
	queue := make([]string, 0, len(target.imports))
	for _, imp := range target.imports {
		queue = append(queue, imp.path)
	}
	for len(queue) > 0 {
		dep := queue[0]
		queue = queue[1:]
		if visibleFiles[dep] {
			continue
		}
		visibleFiles[dep] = true
		facts, err := r.facts(dep)
		if err != nil {
			continue
		}
		for _, imp := range facts.imports {
			if imp.public {
				queue = append(queue, imp.path)
			}
		}
	}

	// Compute the visible symbol set: symbols defined by the target itself
	// or by any visible file. The index is consulted too, so that symbols
	// re-exported through public imports are recognized even when the
	// intermediate file could not be fully analyzed.
	visible := map[string]struct{}{}
	for sym := range target.symbols {
		visible[sym] = struct{}{}
	}
	for dep := range visibleFiles {
		if dep == path {
			continue
		}
		facts, err := r.facts(dep)
		if err != nil {
			continue
		}
		for sym := range facts.symbols {
			visible[sym] = struct{}{}
		}
	}
	p.Index.forEach(func(sym string, files []string) {
		if _, ok := visible[sym]; ok {
			return
		}
		for _, f := range files {
			if visibleFiles[f] {
				visible[sym] = struct{}{}
				return
			}
		}
	})

	// "Known" symbols additionally include everything in the index, so that
	// references to importable symbols anchor scope resolution correctly.
	known := func(sym string) bool {
		if _, ok := visible[sym]; ok {
			return true
		}
		return p.Index.contains(sym)
	}

	// Find the real type references that do not resolve to a visible symbol.
	missing := map[string]struct{}{}
	for _, ref := range typeRefsOf(fdp) {
		fqn := resolveName(ref.name, ref.scope, known)
		if _, ok := visible[fqn]; !ok {
			missing[fqn] = struct{}{}
		}
	}

	plan := &Plan{Path: path}
	directImports := map[string]bool{}
	for _, imp := range target.imports {
		directImports[imp.path] = true
	}
	addSet := map[string]struct{}{}
	for _, sym := range sortedKeys(missing) {
		var usable, cyclic []string
		for _, cand := range p.Index.Files(sym) {
			if cand == path || directImports[cand] {
				continue
			}
			if r.reaches(cand, path) {
				cyclic = append(cyclic, cand)
			} else {
				usable = append(usable, cand)
			}
		}
		slices.Sort(usable)
		slices.Sort(cyclic)
		switch {
		case len(usable) == 1:
			if _, ok := addSet[usable[0]]; !ok {
				addSet[usable[0]] = struct{}{}
				plan.ImportsToAdd = append(plan.ImportsToAdd, usable[0])
			}
		case len(usable) > 1:
			plan.Unresolved = append(plan.Unresolved, Unresolved{
				Symbol:     sym,
				Reason:     ReasonAmbiguous,
				Candidates: usable,
			})
		case len(cyclic) > 0:
			plan.Unresolved = append(plan.Unresolved, Unresolved{
				Symbol:     sym,
				Reason:     ReasonCycle,
				Candidates: cyclic,
			})
		default:
			plan.Unresolved = append(plan.Unresolved, Unresolved{
				Symbol: sym,
				Reason: ReasonNotFound,
			})
		}
	}
	slices.Sort(plan.ImportsToAdd)

	if len(plan.Unresolved) > 0 {
		// The plan cannot produce a compilable file, so it is not usable
		// and no partial revision is emitted.
		return plan, nil
	}
	if len(plan.ImportsToAdd) == 0 {
		// Nothing to do: every reference already resolves.
		plan.Verified = true
		return plan, nil
	}

	revised := insertImports(src, file, plan.ImportsToAdd)
	if err := p.verify(ctx, path, revised); err != nil {
		// The candidate revision did not compile, so the plan is not
		// usable and no revised source is emitted.
		plan.VerifyErr = err
		return plan, nil
	}
	plan.Verified = true
	plan.RevisedSource = revised
	return plan, nil
}

// facts returns the facts for the given file, loading and analyzing it via
// the resolver on first use. The target file's facts are pre-seeded.
func (r *planRunner) facts(path string) (*fileFacts, error) {
	if facts, ok := r.cache[path]; ok {
		if facts == nil {
			return nil, fmt.Errorf("importfix: file %q is unavailable", path)
		}
		return facts, nil
	}
	facts, err := r.load(path)
	if err != nil {
		r.cache[path] = nil
		return nil, err
	}
	r.cache[path] = facts
	return facts, nil
}

func (r *planRunner) load(path string) (*fileFacts, error) {
	res, err := r.planner.Resolver.FindFileByPath(path)
	if err != nil {
		return nil, err
	}
	fdp, err := fileDescriptorProto(path, res)
	if err != nil {
		return nil, err
	}
	return factsFromProto(fdp), nil
}

// reaches reports whether the file at "from" transitively imports the file
// at "to" (following all imports, public or not). It is used to avoid
// suggesting imports that would create a cycle.
func (r *planRunner) reaches(from, to string) bool {
	seen := map[string]bool{}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == to {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		facts, err := r.facts(cur)
		if err != nil {
			continue
		}
		for _, imp := range facts.imports {
			queue = append(queue, imp.path)
		}
	}
	return false
}

// verify re-compiles the revised source of the target file to make sure the
// suggested imports actually produce a compilable file.
func (p *Planner) verify(ctx context.Context, path string, revised []byte) error {
	overlay := protocompile.CompositeResolver{
		protocompile.ResolverFunc(func(name string) (protocompile.SearchResult, error) {
			if name == path {
				return protocompile.SearchResult{Source: bytes.NewReader(revised)}, nil
			}
			return protocompile.SearchResult{}, fmt.Errorf("importfix: unexpected file %q", name)
		}),
		p.Resolver,
	}
	compiler := protocompile.Compiler{Resolver: overlay}
	_, err := compiler.Compile(ctx, path)
	return err
}

// fileDescriptorProto extracts a descriptor proto from a search result,
// parsing source if that is all that is available.
func fileDescriptorProto(path string, res protocompile.SearchResult) (*descriptorpb.FileDescriptorProto, error) {
	switch {
	case res.Desc != nil:
		return protoutil.ProtoFromFileDescriptor(res.Desc), nil
	case res.ParseResult != nil:
		return res.ParseResult.FileDescriptorProto(), nil
	case res.Proto != nil:
		return res.Proto, nil
	case res.AST != nil:
		result, err := parser.ResultFromAST(res.AST, false, reporter.NewHandler(nil))
		if err != nil {
			return nil, err
		}
		return result.FileDescriptorProto(), nil
	case res.Source != nil:
		file, err := parser.Parse(path, res.Source, reporter.NewHandler(nil))
		if err != nil {
			return nil, err
		}
		result, err := parser.ResultFromAST(file, false, reporter.NewHandler(nil))
		if err != nil {
			return nil, err
		}
		return result.FileDescriptorProto(), nil
	default:
		return nil, fmt.Errorf("importfix: resolver returned no usable representation for %q", path)
	}
}

// factsFromProto extracts the package, imports, and defined symbols of a
// file from its descriptor proto.
func factsFromProto(fdp *descriptorpb.FileDescriptorProto) *fileFacts {
	facts := &fileFacts{
		symbols: map[string]struct{}{},
	}
	pkg := fdp.GetPackage()
	public := map[int32]bool{}
	for _, i := range fdp.GetPublicDependency() {
		public[i] = true
	}
	for i, dep := range fdp.GetDependency() {
		facts.imports = append(facts.imports, importRef{path: dep, public: public[int32(i)]})
	}
	add := func(name string) {
		if pkg != "" {
			name = pkg + "." + name
		}
		facts.symbols[name] = struct{}{}
	}
	var addMessage func(prefix string, m *descriptorpb.DescriptorProto)
	addEnum := func(prefix string, e *descriptorpb.EnumDescriptorProto) {
		scope := join(prefix, e.GetName())
		add(scope)
		for _, v := range e.GetValue() {
			add(join(scope, v.GetName()))
		}
	}
	addMessage = func(prefix string, m *descriptorpb.DescriptorProto) {
		scope := join(prefix, m.GetName())
		add(scope)
		for _, f := range m.GetField() {
			add(join(scope, f.GetName()))
		}
		for _, f := range m.GetExtension() {
			add(join(scope, f.GetName()))
		}
		for _, o := range m.GetOneofDecl() {
			add(join(scope, o.GetName()))
		}
		for _, e := range m.GetEnumType() {
			addEnum(scope, e)
		}
		for _, n := range m.GetNestedType() {
			addMessage(scope, n)
		}
	}
	for _, m := range fdp.GetMessageType() {
		addMessage("", m)
	}
	for _, e := range fdp.GetEnumType() {
		addEnum("", e)
	}
	for _, f := range fdp.GetExtension() {
		add(f.GetName())
	}
	for _, s := range fdp.GetService() {
		add(s.GetName())
		for _, m := range s.GetMethod() {
			add(join(s.GetName(), m.GetName()))
		}
	}
	return facts
}

// typeRefsOf collects the real type references of a file from its
// descriptor proto: field types, extendees, and RPC input/output types,
// each with the fully-qualified scope in which it appears.
func typeRefsOf(fdp *descriptorpb.FileDescriptorProto) []typeRef {
	var refs []typeRef
	pkg := fdp.GetPackage()
	addField := func(scope string, f *descriptorpb.FieldDescriptorProto) {
		if name := f.GetTypeName(); name != "" {
			refs = append(refs, typeRef{scope: scope, name: name})
		}
		if ext := f.GetExtendee(); ext != "" {
			refs = append(refs, typeRef{scope: scope, name: ext})
		}
	}
	var walkMessage func(scope string, m *descriptorpb.DescriptorProto)
	walkMessage = func(scope string, m *descriptorpb.DescriptorProto) {
		msgScope := join(scope, m.GetName())
		for _, f := range m.GetField() {
			addField(msgScope, f)
		}
		for _, f := range m.GetExtension() {
			addField(msgScope, f)
		}
		for _, n := range m.GetNestedType() {
			walkMessage(msgScope, n)
		}
	}
	for _, m := range fdp.GetMessageType() {
		walkMessage(pkg, m)
	}
	for _, f := range fdp.GetExtension() {
		addField(pkg, f)
	}
	for _, s := range fdp.GetService() {
		svcScope := join(pkg, s.GetName())
		for _, m := range s.GetMethod() {
			if in := m.GetInputType(); in != "" {
				refs = append(refs, typeRef{scope: svcScope, name: in})
			}
			if out := m.GetOutputType(); out != "" {
				refs = append(refs, typeRef{scope: svcScope, name: out})
			}
		}
	}
	return refs
}

// resolveName resolves a type reference to a fully-qualified name (without
// leading dot) using protobuf scoping rules: the innermost scope in which
// the first component of the name is a known symbol wins.
func resolveName(name, scope string, known func(string) bool) string {
	if strings.HasPrefix(name, ".") {
		return name[1:]
	}
	first := name
	if i := strings.IndexByte(name, '.'); i >= 0 {
		first = name[:i]
	}
	for s := scope; ; {
		if known(join(s, first)) {
			return join(s, name)
		}
		if s == "" {
			return name
		}
		if i := strings.LastIndexByte(s, '.'); i >= 0 {
			s = s[:i]
		} else {
			s = ""
		}
	}
}

// insertImports returns the source with new import declarations inserted.
// Only insertion is performed: existing bytes -- comments and declarations
// included -- are preserved exactly. New imports go after the last existing
// import, or after the package clause, or after the syntax/edition
// declaration, or at the very top of the file, whichever applies first.
func insertImports(src []byte, file *ast.FileNode, imports []string) []byte {
	lines := make([]string, len(imports))
	for i, imp := range imports {
		lines[i] = fmt.Sprintf("import %q;", imp)
	}
	block := strings.Join(lines, "\n") + "\n"

	var anchor ast.Node
	anchorIsImport := false
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.ImportNode:
			anchor, anchorIsImport = decl, true
		case *ast.PackageNode:
			if anchor == nil {
				anchor = decl
			}
		}
	}
	if anchor == nil {
		switch {
		case file.Syntax != nil:
			anchor = file.Syntax
		case file.Edition != nil:
			anchor = file.Edition
		}
	}
	if anchor == nil {
		return []byte(block + "\n" + string(src))
	}
	off := file.NodeInfo(anchor).End().Offset
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	// Advance to just past the end of the line containing the anchor, so
	// that any trailing comment on that line stays with the anchor.
	for off < len(src) && src[off] != '\n' {
		off++
	}
	if off < len(src) {
		off++
	}
	insert := block
	if !anchorIsImport {
		// Separate the new import block from the surrounding declarations.
		insert = "\n" + block
	}
	out := make([]byte, 0, len(src)+len(insert))
	out = append(out, src[:off]...)
	out = append(out, insert...)
	out = append(out, src[off:]...)
	return out
}

func join(scope, name string) string {
	if scope == "" {
		return name
	}
	return scope + "." + name
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
