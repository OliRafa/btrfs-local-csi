// Package driver implements the btrfs-local-csi plugin: it provisions btrfs
// subvolumes with qgroup quotas and bind-mounts them into pods, with no NFS
// hop in between.
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// Name is the CSI driver name. It must match the CSIDriver object and the
// StorageClass provisioner field.
const Name = "btrfs-local-csi"

type Config struct {
	Endpoint string
	NodeID   string
	// Pool is the btrfs directory volumes are provisioned under.
	Pool         string
	Version      string
	Compression  string
	DeletionMode DeletionMode
	// Claims is optional; without it the driver ignores per-PVC annotations.
	Claims ClaimLookup
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Pool == "" {
		return errors.New("a pool directory is required")
	}
	if cfg.DeletionMode == "" {
		cfg.DeletionMode = DeletionRename
	}

	lis, err := listen(cfg.Endpoint)
	if err != nil {
		return err
	}

	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(logRPC))
	csi.RegisterIdentityServer(srv, identity{version: cfg.Version})
	csi.RegisterControllerServer(srv, &controller{
		pool:         cfg.Pool,
		nodeID:       cfg.NodeID,
		compression:  cfg.Compression,
		deletionMode: cfg.DeletionMode,
		claims:       cfg.Claims,
		now:          time.Now,
	})
	csi.RegisterNodeServer(srv, &node{pool: cfg.Pool, nodeID: cfg.NodeID})

	go func() {
		<-ctx.Done()
		slog.Info("shutting down")
		srv.GracefulStop()
	}()

	slog.Info("serving", "endpoint", cfg.Endpoint, "version", cfg.Version, "node", cfg.NodeID)
	return srv.Serve(lis)
}

func listen(endpoint string) (net.Listener, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", endpoint, err)
	}

	switch u.Scheme {
	case "unix":
		// A socket left behind by an unclean exit would make Listen fail with
		// EADDRINUSE even though nothing is serving it.
		if err := os.Remove(u.Path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale socket %q: %w", u.Path, err)
		}
		return net.Listen("unix", u.Path)
	case "tcp":
		return net.Listen("tcp", u.Host)
	default:
		return nil, fmt.Errorf("unsupported endpoint scheme %q", u.Scheme)
	}
}

// logRPC deliberately logs only method names. CSI requests carry secrets in
// their Secrets fields.
func logRPC(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err != nil {
		slog.Error("rpc failed", "method", info.FullMethod, "err", err)
		return resp, err
	}
	slog.Debug("rpc served", "method", info.FullMethod)
	return resp, nil
}
