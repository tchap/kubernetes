/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package app

import "testing"

func TestHookList(t *testing.T) {
	t.Run("execute empty list", func(t *testing.T) {
		var hooks hookList
		hooks.Execute()
	})

	t.Run("execute method only ever calls hooks once", func(t *testing.T) {
		var counter int
		increment := func() { counter++ }

		var hooks hookList
		hooks.Register(increment)

		hooks.Execute()
		hooks.Execute()

		if counter != 1 {
			t.Error("hook should have been called once")
		}
	})
}
