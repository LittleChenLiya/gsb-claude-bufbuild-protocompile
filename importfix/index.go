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
	"slices"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// SymbolIndex maps fully-qualified protobuf symbol names (without a leading
// dot, e.g. "foo.Bar") to the paths of the files that define them. It is the
// source of candidate imports when a file references a symbol that is not
// already visible through its existing imports.
type SymbolIndex struct {
	bySymbol map[string][]string
}

// NewSymbolIndex returns an empty symbol index.
func NewSymbolIndex() *SymbolIndex {
	return &SymbolIndex{bySymbol: map[string][]string{}}
}

// Add records that the file at the given path defines the given
// fully-qualified symbol. A leading dot on the symbol is optional.
func (x *SymbolIndex) Add(symbol, path string) {
	symbol = strings.TrimPrefix(symbol, ".")
	files := x.bySymbol[symbol]
	if !slices.Contains(files, path) {
		x.bySymbol[symbol] = append(files, path)
	}
}

// AddFile records every symbol defined in the given file descriptor,
// including nested elements, under the file's path.
func (x *SymbolIndex) AddFile(fd protoreflect.FileDescriptor) {
	path := fd.Path()
	addFields := func(fs protoreflect.FieldDescriptors) {
		for i := 0; i < fs.Len(); i++ {
			x.Add(string(fs.Get(i).FullName()), path)
		}
	}
	addExts := func(es protoreflect.ExtensionDescriptors) {
		for i := 0; i < es.Len(); i++ {
			x.Add(string(es.Get(i).FullName()), path)
		}
	}
	var addEnum func(e protoreflect.EnumDescriptor)
	var addMessage func(m protoreflect.MessageDescriptor)
	addMessage = func(m protoreflect.MessageDescriptor) {
		x.Add(string(m.FullName()), path)
		addFields(m.Fields())
		addExts(m.Extensions())
		for i := 0; i < m.Oneofs().Len(); i++ {
			x.Add(string(m.Oneofs().Get(i).FullName()), path)
		}
		for i := 0; i < m.Enums().Len(); i++ {
			addEnum(m.Enums().Get(i))
		}
		for i := 0; i < m.Messages().Len(); i++ {
			addMessage(m.Messages().Get(i))
		}
	}
	addEnum = func(e protoreflect.EnumDescriptor) {
		x.Add(string(e.FullName()), path)
		for i := 0; i < e.Values().Len(); i++ {
			x.Add(string(e.Values().Get(i).FullName()), path)
		}
	}
	for i := 0; i < fd.Messages().Len(); i++ {
		addMessage(fd.Messages().Get(i))
	}
	for i := 0; i < fd.Enums().Len(); i++ {
		addEnum(fd.Enums().Get(i))
	}
	addExts(fd.Extensions())
	for i := 0; i < fd.Services().Len(); i++ {
		svc := fd.Services().Get(i)
		x.Add(string(svc.FullName()), path)
		for j := 0; j < svc.Methods().Len(); j++ {
			x.Add(string(svc.Methods().Get(j).FullName()), path)
		}
	}
}

// Files returns the paths of the files known to define the given
// fully-qualified symbol, in undefined order. A leading dot is optional.
func (x *SymbolIndex) Files(symbol string) []string {
	return x.bySymbol[strings.TrimPrefix(symbol, ".")]
}

// contains reports whether the symbol is present in the index.
func (x *SymbolIndex) contains(symbol string) bool {
	_, ok := x.bySymbol[symbol]
	return ok
}

// forEach invokes the given function for every symbol in the index.
func (x *SymbolIndex) forEach(fn func(symbol string, files []string)) {
	for sym, files := range x.bySymbol {
		fn(sym, files)
	}
}
