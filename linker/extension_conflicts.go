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

package linker

import (
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/protoutil"
	"github.com/bufbuild/protocompile/reporter"
	"github.com/bufbuild/protocompile/walk"
)

// CheckExtensionNumberConflicts examines the given files and reports an error
// for every extension field number that is declared for the same extendee
// message more than once. Declarations are merged by the fully-qualified
// identity of the extendee message: two extensions conflict only if they
// extend the same message (by full name) and use the same field number.
// Extensions of different messages, or extensions of the same message that
// use different numbers, are not conflicts.
//
// This is useful when files are linked separately (for example, with separate
// calls to Link that do not share a Symbols table): in that case, the normal
// link step cannot detect that two files declare conflicting extensions for a
// common extendee. This check can then be run over the linked results, before
// generating descriptors from them. It only reports diagnostics; it does not
// modify the given files, their ASTs, or any other linker output.
//
// The reported error indicates the location of the conflicting extension
// number as well as the location of the first declaration of the same number
// for the same message.
//
// Diagnostics are reported via the given handler. If any error is reported,
// this function returns a non-nil error.
func CheckExtensionNumberConflicts(handler *reporter.Handler, files ...File) error {
	type extDecl struct {
		span ast.SourceSpan
	}
	decls := map[extNumber][]extDecl{}
	var order []extNumber
	seenFiles := map[string]struct{}{}
	for _, f := range files {
		if f == nil {
			continue
		}
		if _, ok := seenFiles[f.Path()]; ok {
			// already examined this file
			continue
		}
		seenFiles[f.Path()] = struct{}{}
		err := walk.Descriptors(f, func(d protoreflect.Descriptor) error {
			fd, ok := d.(protoreflect.FieldDescriptor)
			if !ok || !fd.IsExtension() {
				return nil
			}
			key := extNumber{
				extendee: fd.ContainingMessage().FullName(),
				tag:      fd.Number(),
			}
			if _, ok := decls[key]; !ok {
				order = append(order, key)
			}
			decls[key] = append(decls[key], extDecl{
				span: extensionNumberSpan(fd),
			})
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, key := range order {
		group := decls[key]
		if len(group) < 2 {
			continue
		}
		first := group[0]
		for _, dupe := range group[1:] {
			err := handler.HandleErrorf(dupe.span, "extension with tag %d for message %s already defined at %v",
				key.tag, key.extendee, first.span.Start())
			if err != nil {
				return err
			}
		}
	}
	return handler.Error()
}

// extensionNumberSpan returns the source span of the given extension field's
// tag number. If the field's file is a linked result with source information,
// the span is computed from the file's AST. Otherwise, it is computed from
// the file's source code info, if present.
func extensionNumberSpan(fd protoreflect.FieldDescriptor) ast.SourceSpan {
	if res, ok := fd.ParentFile().(*result); ok && res.hasSource() {
		if node := res.FieldNode(protoutil.ProtoFromFieldDescriptor(fd)); node != nil {
			return res.FileNode().NodeInfo(node.FieldTag())
		}
	}
	return sourceSpanForNumber(fd)
}
