// The default entry point hosts the service and its native HTTP client together.
package main

import (
	"context"
	"fmt"
	"os"

	"guludrive/app"
	"guludrive/app/command"
	workspace "guludrive/ui"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "GuluDrive:", err)
		showStartupError()
		os.Exit(1)
	}
}

func run(args []string) error {
	return command.Run(args, app.Version, func(ctx context.Context, serverURL string) error {
		return workspace.Run(workspace.Options{ServerURL: serverURL, Context: ctx})
	})
}
