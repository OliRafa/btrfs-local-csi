package driver_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OliRafa/btrfs-local-csi/internal/driver"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
)

// TestCSISanity runs the upstream CSI conformance suite against a live driver
// backed by the loopback pool. It is the reason writing a storage driver is a
// reasonable thing to do: the idempotency and error-code edges it exercises are
// exactly the ones that are easy to get wrong by hand.
func TestCSISanity(t *testing.T) {
	root := os.Getenv("BTRFS_TEST_POOL")
	if root == "" {
		t.Skip("BTRFS_TEST_POOL is unset; skipping tests that need a real btrfs filesystem")
	}

	pool := filepath.Join(root, "sanity")
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// The socket lives outside the pool so the suite cannot trip over it.
	endpoint := "unix://" + filepath.Join(t.TempDir(), "csi.sock")

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- driver.Run(ctx, driver.Config{
			Endpoint: endpoint,
			NodeID:   "sanity-node",
			Pool:     pool,
			Version:  "v0.0.0-sanity",
			// Destroy rather than move to trash, so the suite's own cleanup
			// actually reclaims the pool.
			DeletionMode: driver.DeletionDelete,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("driver: %v", err)
		}
	})

	waitForSocket(t, endpoint)

	config := sanity.NewTestConfig()
	config.Address = endpoint
	config.TargetPath = filepath.Join(t.TempDir(), "target")
	config.StagingPath = filepath.Join(t.TempDir(), "staging")

	sanity.Test(t, config)
}

func waitForSocket(t *testing.T, endpoint string) {
	t.Helper()

	path := endpoint[len("unix://"):]
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("driver did not create %s within the deadline", path)
}
