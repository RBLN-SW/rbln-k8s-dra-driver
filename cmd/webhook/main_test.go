package main

import (
	"io"
	"strings"
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

func TestDriverNameOverrideRejected(t *testing.T) {
	app := newApp("info", "json")
	app.Writer = io.Discard
	app.ErrWriter = io.Discard
	err := app.Run([]string{"webhook", "--driver-name=npu.example.com"})
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -driver-name") {
		t.Fatalf("removed driver-name flag: %v", err)
	}
}
