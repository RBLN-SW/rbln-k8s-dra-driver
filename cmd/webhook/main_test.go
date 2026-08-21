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
