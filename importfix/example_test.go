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

package importfix_test

import (
	"context"
	"fmt"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/importfix"
)

// A three-file example: app/main.proto references foo.Bar, which is defined
// in foo/foo.proto, while middle/middle.proto re-exports foo/foo.proto via a
// public import. The file app/main.proto forgot its import, so the planner
// suggests the one file that uniquely provides foo.Bar.
func ExamplePlanner_Plan() {
	index := importfix.NewSymbolIndex()
	index.Add("foo.Bar", "foo/foo.proto")
	index.Add("middle.Wrapper", "middle/middle.proto")

	planner := &importfix.Planner{
		Resolver: &protocompile.SourceResolver{ImportPaths: []string{"testdata"}},
		Index:    index,
	}
	plan, err := planner.Plan(context.Background(), "app/main.proto")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("imports to add:", plan.ImportsToAdd)
	fmt.Println("unresolved:", plan.Unresolved)
	fmt.Println("verified:", plan.Verified)
	fmt.Println()
	fmt.Print(string(plan.RevisedSource))

	// Output:
	// imports to add: [foo/foo.proto]
	// unresolved: []
	// verified: true
	//
	// syntax = "proto3";
	//
	// package app;
	//
	// import "foo/foo.proto";
	//
	// // Uses foo.Bar but forgets to import "foo/foo.proto".
	// message Uses {
	//   foo.Bar bar = 1;
	// }
}
