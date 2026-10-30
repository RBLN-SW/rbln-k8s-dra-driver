/*
Copyright 2026 Rebellions Inc.

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

package main

import (
	"io"
	"testing"
)

// The app must build its flag set and render help without the deleted
// component-base logging flags.
func TestNewAppRunsHelp(t *testing.T) {
	app := newApp("info", "json")
	app.Writer = io.Discard
	if err := app.Run([]string{"webhook", "--help"}); err != nil {
		t.Fatalf("--help: %v", err)
	}
}
