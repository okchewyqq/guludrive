package main

import (
	"context"
	"os"
	"testing"

	"guludrive/app/command"
)

func TestHelpVersionAndRejectedArgumentsHaveNoSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Chdir(home)
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	openUI := func(context.Context, string) error {
		t.Fatal("non-serving arguments opened the native UI")
		return nil
	}
	for _, args := range [][]string{{"-help"}, {"-h"}, {"--help"}, {"-version"}, {"--version"}} {
		if err := command.Run(args, "offline-test", openUI); err != nil {
			t.Fatal("help/version initialized a service", err)
		}
	}
	for _, args := range [][]string{
		{"serve"}, {"set-password"}, {"help"}, {"version"},
		{"-config", "missing.json"}, {"-data-dir", home}, {"-listen", "127.0.0.1:0"},
		{"-username", "operator"}, {"-password", "fixture"}, {"-password-stdin"},
		{"-proxy", "http://127.0.0.1:1"}, {"-public-origin", "https://example.invalid"},
		{"-max-file-size", "0"}, {"-ui-dir", home},
	} {
		if err := command.Run(args, "offline-test", openUI); err == nil {
			t.Fatalf("removed command/flag was accepted: %q", args[0])
		}
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("non-serving arguments created private state", err)
	}
}
