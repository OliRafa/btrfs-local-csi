package driver

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
)

func readVolumeQuota(t *testing.T, path string) volumeQuota {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var quota volumeQuota
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return quota
}

func TestQuotaStatePublishesPerVolumeFiles(t *testing.T) {
	ctx, c := newController(t)

	const capacity = 64 << 20
	if _, err := c.CreateVolume(ctx, createRequest("pvc-a", "localflix", "library", capacity)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := c.CreateVolume(ctx, createRequest("pvc-b", "bookstore", "comics", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	state := &quotaState{pool: c.pool, dir: t.TempDir()}
	if err := state.publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}

	quota := readVolumeQuota(t, filepath.Join(state.dir, "localflix", "library.json"))
	if quota.QuotaBytes != capacity {
		t.Errorf("quota_bytes = %d, want %d", quota.QuotaBytes, capacity)
	}
	if quota.AvailBytes <= 0 || quota.AvailBytes > capacity {
		t.Errorf("avail_bytes = %d, want a positive value no larger than %d", quota.AvailBytes, capacity)
	}

	if _, err := os.Stat(filepath.Join(state.dir, "bookstore", "comics.json")); err != nil {
		t.Errorf("second volume not published: %v", err)
	}
}

// An over-quota volume must report zero available, not a negative number: the
// interposer feeds this into statvfs, whose block counts are unsigned, so a
// negative would wrap into terabytes of imaginary free space.
func TestQuotaStateClampsOverQuotaVolumes(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-a", "localflix", "library", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	path := filepath.Join(c.pool, "localflix", "library")

	// Random rather than zeroes: volumes are created with compression=zstd, and
	// 8 MiB of zeroes lands as a couple of hundred KiB, never exceeding the
	// limit this test needs to breach.
	payload := make([]byte, 8<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), payload, 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := btrfs.Sync(ctx, path); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// Drop the limit below what is already stored.
	if err := btrfs.SetQuota(ctx, path, 1<<20); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	state := &quotaState{pool: c.pool, dir: t.TempDir()}
	if err := state.publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}

	quota := readVolumeQuota(t, filepath.Join(state.dir, "localflix", "library.json"))
	if quota.AvailBytes != 0 {
		t.Errorf("avail_bytes = %d, want 0 for an over-quota volume", quota.AvailBytes)
	}
}

func TestQuotaStateIgnoresUnstampedDirectories(t *testing.T) {
	ctx, c := newController(t)

	stray := filepath.Join(c.pool, "localflix", "not-ours")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	state := &quotaState{pool: c.pool, dir: t.TempDir()}
	if err := state.publish(ctx); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := os.Stat(filepath.Join(state.dir, "localflix", "not-ours.json")); !os.IsNotExist(err) {
		t.Errorf("published state for a directory the driver does not own: %v", err)
	}
}

// A deleted volume's state has to be withdrawn, not merely left unrefreshed.
// The interposer cannot tell a stale file from a current one, so it would keep
// serving a dead volume's numbers instead of falling back to the real
// filesystem — and a volume that was full when it went away would report
// nothing free, forever.
func TestQuotaStatePrunesDeletedVolumes(t *testing.T) {
	ctx, c := newController(t)

	for _, name := range []string{"library", "comics"} {
		if _, err := c.CreateVolume(ctx, createRequest("pvc-"+name, "localflix", name, 32<<20)); err != nil {
			t.Fatalf("CreateVolume %s: %v", name, err)
		}
	}

	state := &quotaState{pool: c.pool, dir: t.TempDir()}
	if err := state.publish(ctx); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	// Both must be published first, or a later absence proves nothing.
	for _, name := range []string{"library", "comics"} {
		if _, err := os.Stat(filepath.Join(state.dir, "localflix", name+".json")); err != nil {
			t.Fatalf("%s was not published to begin with: %v", name, err)
		}
	}

	if _, err := c.DeleteVolume(ctx, deleteRequest("localflix/library")); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if err := state.publish(ctx); err != nil {
		t.Fatalf("second publish: %v", err)
	}

	if _, err := os.Stat(filepath.Join(state.dir, "localflix", "library.json")); !os.IsNotExist(err) {
		t.Errorf("state for the deleted volume survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state.dir, "localflix", "comics.json")); err != nil {
		t.Errorf("the surviving volume lost its state: %v", err)
	}
}
