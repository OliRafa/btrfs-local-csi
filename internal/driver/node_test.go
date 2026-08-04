package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newPublishedVolume provisions a volume and bind-mounts it, returning the node
// service, the volume handle and the target path.
func newPublishedVolume(t *testing.T, capacity int64, readonly bool) (context.Context, *node, string, string) {
	t.Helper()

	ctx, c := newController(t)
	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", capacity)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	n := &node{pool: c.pool, nodeID: "test-node"}
	const handle = "localflix/library"
	target := filepath.Join(t.TempDir(), "publish")

	// Unmount before the pool cleanup runs: btrfs refuses to delete a
	// subvolume that is still bind-mounted somewhere.
	t.Cleanup(func() {
		if _, err := n.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
			VolumeId: handle, TargetPath: target,
		}); err != nil {
			t.Errorf("cleanup NodeUnpublishVolume: %v", err)
		}
	})

	if _, err := n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:         handle,
		TargetPath:       target,
		VolumeCapability: mountCapability(),
		Readonly:         readonly,
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	return ctx, n, handle, target
}

func TestNodePublishExposesTheSubvolume(t *testing.T) {
	ctx, n, _, target := newPublishedVolume(t, 64<<20, false)

	mounted, err := isMountPoint(target)
	if err != nil {
		t.Fatalf("isMountPoint: %v", err)
	}
	if !mounted {
		t.Fatal("target is not a mount point after publish")
	}

	// A file written through the target must appear in the subvolume itself.
	if err := os.WriteFile(filepath.Join(target, "canary"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write through target: %v", err)
	}
	source, err := ResolvedVolumePath(n.pool, "localflix/library")
	if err != nil {
		t.Fatalf("ResolvedVolumePath: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "canary")); err != nil {
		t.Errorf("file written through the mount is not in the subvolume: %v", err)
	}
	_ = ctx
}

func TestNodePublishIsIdempotent(t *testing.T) {
	ctx, n, handle, target := newPublishedVolume(t, 32<<20, false)

	if _, err := n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:         handle,
		TargetPath:       target,
		VolumeCapability: mountCapability(),
	}); err != nil {
		t.Fatalf("second NodePublishVolume: %v", err)
	}
}

func TestNodePublishReadonlyRejectsWrites(t *testing.T) {
	_, _, _, target := newPublishedVolume(t, 32<<20, true)

	err := os.WriteFile(filepath.Join(target, "canary"), []byte("hello"), 0o644)
	if !errors.Is(err, unix.EROFS) {
		t.Fatalf("write to a read-only publish = %v, want EROFS", err)
	}
}

func TestNodePublishRejectsMissingVolume(t *testing.T) {
	ctx, c := newController(t)
	n := &node{pool: c.pool, nodeID: "test-node"}

	_, err := n.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:         "localflix/never-existed",
		TargetPath:       filepath.Join(t.TempDir(), "publish"),
		VolumeCapability: mountCapability(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("NodePublishVolume for a missing volume = %v, want NotFound", err)
	}
}

func TestNodeUnpublishIsIdempotent(t *testing.T) {
	ctx, c := newController(t)
	n := &node{pool: c.pool, nodeID: "test-node"}

	if _, err := n.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "localflix/library",
		TargetPath: filepath.Join(t.TempDir(), "never-mounted"),
	}); err != nil {
		t.Fatalf("NodeUnpublishVolume on an unmounted target = %v, want success", err)
	}
}

// The reason the driver exists: statvfs reports the whole pool, so stats have
// to come from the qgroup instead.
func TestNodeGetVolumeStatsReportsQuotaNotPool(t *testing.T) {
	const capacity = 64 << 20
	ctx, n, handle, target := newPublishedVolume(t, capacity, false)

	if err := os.WriteFile(filepath.Join(target, "payload"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	resp, err := n.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId: handle, VolumePath: target,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}

	usage := resp.GetUsage()
	if len(usage) != 1 {
		t.Fatalf("got %d usage entries, want 1", len(usage))
	}
	if usage[0].GetUnit() != csi.VolumeUsage_BYTES {
		t.Errorf("unit = %v, want BYTES", usage[0].GetUnit())
	}
	if usage[0].GetTotal() != capacity {
		t.Errorf("total = %d, want the qgroup limit %d", usage[0].GetTotal(), capacity)
	}
	if got := usage[0].GetUsed() + usage[0].GetAvailable(); got != capacity {
		t.Errorf("used + available = %d, want %d", got, capacity)
	}
}

func TestNodeGetVolumeStatsRejectsMissingVolume(t *testing.T) {
	ctx, c := newController(t)
	n := &node{pool: c.pool, nodeID: "test-node"}

	_, err := n.NodeGetVolumeStats(ctx, &csi.NodeGetVolumeStatsRequest{
		VolumeId: "localflix/library", VolumePath: filepath.Join(t.TempDir(), "gone"),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("NodeGetVolumeStats for a missing path = %v, want NotFound", err)
	}
}

func TestNodeGetInfoAdvertisesTopology(t *testing.T) {
	n := &node{pool: t.TempDir(), nodeID: "test-node"}

	resp, err := n.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo: %v", err)
	}
	if resp.GetNodeId() != "test-node" {
		t.Errorf("node id = %q, want %q", resp.GetNodeId(), "test-node")
	}
	if got := resp.GetAccessibleTopology().GetSegments()[TopologyNodeKey]; got != "test-node" {
		t.Errorf("topology segment %s = %q, want %q", TopologyNodeKey, got, "test-node")
	}
}

func TestUnescapeMountPoint(t *testing.T) {
	if got, want := unescapeMountPoint(`/mnt/with\040space`), "/mnt/with space"; got != want {
		t.Errorf("unescapeMountPoint = %q, want %q", got, want)
	}
}
