// Command repodepot-agent is The Repo Depot's agent: it installs, updates
// and removes software on this machine when the server asks.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/AC3Labs/RepoDepot/internal/agent"
)

func main() {
	configPath := flag.String("config", agent.DefaultConfigPath(), "path to config.json")
	flag.Parse()
	switch flag.Arg(0) {
	case "version", "--version":
		fmt.Println(agent.Version)
		return
	case "install-service", "uninstall-service":
		if err := serviceCommand(flag.Arg(0), *configPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if runAsService(*configPath) {
		return
	}
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agent.New(cfg).Run(ctx); err != nil {
		log.Fatal(err)
	}
}
