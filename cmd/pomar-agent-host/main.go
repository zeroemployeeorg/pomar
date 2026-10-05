// Command pomar-agent-host serves development agent environments independently
// of the permanent Pomar CI manager.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

func main() {
	file := flag.String("config", "", "owner-supplied development configuration")
	flag.Parse()
	config, err := agentenv.ReadHostConfig(*file)
	if err == nil {
		var host *agentenv.Host
		host, err = agentenv.OpenHost(config)
		if err == nil {
			defer host.Close()
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			err = host.Serve(ctx)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pomar-agent-host:", err)
		os.Exit(1)
	}
}
