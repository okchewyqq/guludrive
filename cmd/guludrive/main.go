// Command guludrive serves Web/API/DAV without linking a native UI framework.
package main

import (
	"log"
	"os"

	"guludrive/app"
	"guludrive/app/command"
)

func main() {
	if err := command.Run(os.Args[1:], app.Version, nil); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
