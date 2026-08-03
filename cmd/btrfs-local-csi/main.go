package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/OliRafa/btrfs-local-csi/internal/driver"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		cfg         driver.Config
		showVersion bool
		debug       bool
	)
	flag.StringVar(&cfg.Endpoint, "endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint to listen on")
	flag.StringVar(&cfg.NodeID, "node-id", "", "Identifier of the node this instance runs on")
	flag.StringVar(&cfg.Pool, "pool", "", "btrfs directory that volumes are provisioned under")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit")
	flag.BoolVar(&debug, "debug", false, "Log every served RPC")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	if debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	cfg.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := driver.Run(ctx, cfg); err != nil {
		slog.Error("driver exited", "err", err)
		os.Exit(1)
	}
}
