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

package protocompile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
)

// TestCrossFileExtensionNumberConflict verifies that the compiler rejects
// extensions declared in different files that use the same field number for
// the same extendee, while allowing extensions that differ in number or
// extendee. The diagnostic is produced during linking, before descriptors
// are generated, and reports the locations of both conflicting declarations.
func TestCrossFileExtensionNumberConflict(t *testing.T) {
	t.Parallel()

	newCompiler := func() *protocompile.Compiler {
		return &protocompile.Compiler{
			Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
				ImportPaths: []string{"testdata"},
			}),
		}
	}

	t.Run("same_extendee_same_number_rejected", func(t *testing.T) {
		t.Parallel()
		_, err := newCompiler().Compile(
			t.Context(),
			"extconflict/ext_a.proto",
			"extconflict/ext_b_same_number.proto",
		)
		require.Error(t, err)
		// The diagnostic identifies the extendee and number and reports the
		// locations of both conflicting declarations, one in each file. The
		// files are compiled concurrently, so either one may be reported as
		// the pre-existing declaration.
		assert.Contains(t, err.Error(), "extension with tag 10 for message extconflict.Foo already defined at")
		assert.Contains(t, err.Error(), "extconflict/ext_a.proto")
		assert.Contains(t, err.Error(), "extconflict/ext_b_same_number.proto")
	})

	t.Run("same_extendee_different_number_ok", func(t *testing.T) {
		t.Parallel()
		files, err := newCompiler().Compile(
			t.Context(),
			"extconflict/ext_a.proto",
			"extconflict/ext_b_different_number.proto",
		)
		require.NoError(t, err)
		require.Len(t, files, 2)
		// Normal linking output is unaffected: both extensions are present.
		extA := findExtension(t, files, "extconflict.ext_a")
		assert.EqualValues(t, 10, extA.Number())
		extB := findExtension(t, files, "extconflict.ext_b")
		assert.EqualValues(t, 11, extB.Number())
	})

	t.Run("different_extendee_same_number_ok", func(t *testing.T) {
		t.Parallel()
		files, err := newCompiler().Compile(
			t.Context(),
			"extconflict/ext_a.proto",
			"extconflict/ext_b_different_extendee.proto",
		)
		require.NoError(t, err)
		require.Len(t, files, 2)
		extA := findExtension(t, files, "extconflict.ext_a")
		assert.Equal(t, protoreflect.FullName("extconflict.Foo"), extA.ContainingMessage().FullName())
		extB := findExtension(t, files, "extconflict.ext_b")
		assert.Equal(t, protoreflect.FullName("extconflict.Bar"), extB.ContainingMessage().FullName())
		assert.EqualValues(t, 10, extB.Number())
	})
}

func findExtension(t *testing.T, files linker.Files, name protoreflect.FullName) protoreflect.FieldDescriptor {
	t.Helper()
	for _, file := range files {
		if d := file.FindDescriptorByName(name); d != nil {
			ext, ok := d.(protoreflect.FieldDescriptor)
			require.True(t, ok, "%s is not an extension field", name)
			require.True(t, ext.IsExtension(), "%s is not an extension field", name)
			return ext
		}
	}
	t.Fatalf("extension %s not found", name)
	return nil
}
