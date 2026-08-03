package btrfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// newSubvolume creates a subvolume under the test pool and schedules its
// removal. It skips the test when no real btrfs filesystem is available.
func newSubvolume(t *testing.T) (context.Context, string) {
	t.Helper()

	pool := os.Getenv("BTRFS_TEST_POOL")
	if pool == "" {
		t.Skip("BTRFS_TEST_POOL is unset; skipping tests that need a real btrfs filesystem")
	}

	ctx := t.Context()
	path := filepath.Join(pool, "test-"+t.Name())
	if err := CreateSubvolume(ctx, path); err != nil {
		t.Fatalf("CreateSubvolume: %v", err)
	}
	t.Cleanup(func() {
		if err := DeleteSubvolume(context.WithoutCancel(ctx), path); err != nil {
			t.Errorf("cleanup DeleteSubvolume(%s): %v", path, err)
		}
	})
	return ctx, path
}

func TestCreatedPathIsASubvolume(t *testing.T) {
	_, path := newSubvolume(t)

	isSubvol, err := IsSubvolume(path)
	if err != nil {
		t.Fatalf("IsSubvolume: %v", err)
	}
	if !isSubvol {
		t.Error("freshly created subvolume not recognised as one")
	}
}

func TestPlainDirectoryIsNotASubvolume(t *testing.T) {
	_, path := newSubvolume(t)

	plain := filepath.Join(path, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	isSubvol, err := IsSubvolume(plain)
	if err != nil {
		t.Fatalf("IsSubvolume: %v", err)
	}
	if isSubvol {
		t.Error("plain directory reported as a subvolume")
	}
}

func TestQuotaUsageReportsLimitAndConsumption(t *testing.T) {
	ctx, path := newSubvolume(t)

	const limit = 100 << 20
	if err := SetQuota(ctx, path, limit); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "payload"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := Sync(ctx, path); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	usage, err := QuotaUsage(ctx, path)
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	if usage.Limit != limit {
		t.Errorf("Limit = %d, want %d", usage.Limit, limit)
	}
	if usage.Referenced <= 0 {
		t.Errorf("Referenced = %d, want a positive value after writing 4 MiB", usage.Referenced)
	}
	if usage.Available() != limit-usage.Referenced {
		t.Errorf("Available() = %d, want %d", usage.Available(), limit-usage.Referenced)
	}
}

func TestQuotaIsEnforced(t *testing.T) {
	ctx, path := newSubvolume(t)

	if err := SetQuota(ctx, path, 8<<20); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}

	// Write past the limit in chunks; btrfs reserves space per write, so the
	// rejection surfaces partway through rather than up front.
	var werr error
	for i := range 12 {
		name := filepath.Join(path, fmt.Sprintf("fill-%02d", i))
		if werr = os.WriteFile(name, make([]byte, 2<<20), 0o644); werr != nil {
			break
		}
		_ = Sync(ctx, path)
	}
	if werr == nil {
		t.Fatal("wrote 24 MiB into an 8 MiB quota without error")
	}
	if !errors.Is(werr, unix.EDQUOT) && !errors.Is(werr, unix.ENOSPC) {
		t.Fatalf("write failed with %v, want EDQUOT or ENOSPC", werr)
	}
}

func TestQuotaUsageWithoutLimitReportsZero(t *testing.T) {
	ctx, path := newSubvolume(t)

	usage, err := QuotaUsage(ctx, path)
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	if usage.Limit != 0 {
		t.Errorf("Limit = %d, want 0 when unset", usage.Limit)
	}
	if usage.Available() != 0 {
		t.Errorf("Available() = %d, want 0 when no limit is set", usage.Available())
	}
}

func TestAvailableClampsWhenOverLimit(t *testing.T) {
	over := Usage{Referenced: 3 << 40, Limit: 2 << 40}
	if got := over.Available(); got != 0 {
		t.Errorf("Available() = %d, want 0 when referenced exceeds the limit", got)
	}
}

func TestSetCompression(t *testing.T) {
	ctx, path := newSubvolume(t)

	if err := SetCompression(ctx, path, "zstd"); err != nil {
		t.Fatalf("SetCompression: %v", err)
	}

	algorithm, err := Compression(ctx, path)
	if err != nil {
		t.Fatalf("Compression: %v", err)
	}
	if algorithm != "zstd" {
		t.Errorf("compression = %q, want %q", algorithm, "zstd")
	}
}
