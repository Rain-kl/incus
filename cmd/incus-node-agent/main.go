package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxc/incus/v7/internal/server/agent/config"
	"github.com/lxc/incus/v7/internal/server/agent/connector"
)

func main() {
	path := flag.String("config", connector.ConfigPath, "connection configuration")
	socket := flag.String("incus-socket", "", "local Incus socket")
	once := flag.Bool("once", false, "send one heartbeat and exit")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		log.Print("incus-agent " + connector.Version)
		return
	}
	cfg, err := config.Load(*path)
	if err != nil {
		log.Fatal("Invalid agent configuration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err = connector.Run(ctx, cfg, *socket, *once); err != nil {
		log.Fatal(err)
	}
}
