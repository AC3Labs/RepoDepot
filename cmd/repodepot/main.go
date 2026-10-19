// Command repodepot is The Repo Depot server: the web app that finds
// software on GitHub, installs it on your machines and keeps it updated.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"repodepot/internal/server"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(server.Version)
		return
	}
	s, err := server.New(server.ConfigFromEnv())
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
