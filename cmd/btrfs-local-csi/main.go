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
		cfg          driver.Config
		deletionMode string
		showVersion  bool
		debug        bool
	)
	flag.StringVar(&cfg.Endpoint, "endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint to listen on")
	flag.StringVar(&cfg.NodeID, "node-id", "", "Identifier of the node this instance runs on")
	flag.StringVar(&cfg.Pool, "pool", "", "btrfs directory that volumes are provisioned under")
	flag.StringVar(&cfg.Compression, "compression", "zstd", "btrfs compression to set on new volumes, or empty to leave unset")
	flag.StringVar(&deletionMode, "deletion-mode", string(driver.DeletionRename),
		`What DeleteVolume does: "rename" moves the volume into <pool>/.trash, "delete" destroys it`)
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit")
	flag.BoolVar(&debug, "debug", false, "Log every served RPC")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	mode, err := driver.ParseDeletionMode(deletionMode)
	if err != nil {
		slog.Error("invalid flag", "flag", "deletion-mode", "err", err)
		os.Exit(2)
	}
	cfg.DeletionMode = mode

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
