package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
)

// quotaState publishes each volume's qgroup numbers as JSON on the node.
//
// btrfs deliberately does not report qgroup limits through statvfs, so an
// application inside a pod sees the whole pool's free space however small its
// volume is. That is true of bind mounts and NFS alike and there is no upstream
// fix; the gap has been documented since 2016. The workaround is an LD_PRELOAD
// interposer that answers statvfs from these files.
//
// The shape is fixed by the existing interposer and must not change:
//
//	{"quota_bytes":N,"avail_bytes":N}
type quotaState struct {
	pool     string
	dir      string
	interval time.Duration
}

type volumeQuota struct {
	QuotaBytes int64 `json:"quota_bytes"`
	AvailBytes int64 `json:"avail_bytes"`
}

func (q *quotaState) run(ctx context.Context) {
	ticker := time.NewTicker(q.interval)
	defer ticker.Stop()

	for {
		if err := q.publish(ctx); err != nil {
			slog.Error("could not publish quota state", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (q *quotaState) publish(ctx context.Context) error {
	namespaces, err := os.ReadDir(q.pool)
	if err != nil {
		return fmt.Errorf("read pool %q: %w", q.pool, err)
	}

	for _, namespace := range namespaces {
		// Skips .trash, which is unreachable as a handle for the same reason.
		if !namespace.IsDir() || strings.HasPrefix(namespace.Name(), ".") {
			continue
		}

		volumes, err := os.ReadDir(filepath.Join(q.pool, namespace.Name()))
		if err != nil {
			slog.Warn("could not read namespace directory", "namespace", namespace.Name(), "err", err)
			continue
		}

		for _, volume := range volumes {
			if !volume.IsDir() {
				continue
			}
			path := filepath.Join(q.pool, namespace.Name(), volume.Name())

			// Anything without a claim stamp is not ours to report on.
			if _, err := readClaim(path); err != nil {
				continue
			}
			usage, err := btrfs.QuotaUsage(ctx, path)
			if err != nil {
				slog.Warn("could not read quota", "path", path, "err", err)
				continue
			}
			if err := q.write(namespace.Name(), volume.Name(), usage); err != nil {
				slog.Warn("could not write quota state", "path", path, "err", err)
			}
		}
	}
	return nil
}

func (q *quotaState) write(namespace, name string, usage btrfs.Usage) error {
	dir := filepath.Join(q.dir, namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	payload, err := json.Marshal(volumeQuota{
		QuotaBytes: usage.Limit,
		AvailBytes: usage.Available(),
	})
	if err != nil {
		return err
	}

	// Written and renamed rather than truncated in place: the interposer reads
	// this on every statvfs, and a partial read would hand the application a
	// nonsense free-space figure.
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name+".json"))
}
