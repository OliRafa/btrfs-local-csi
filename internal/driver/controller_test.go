package driver

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var testClock = func() time.Time { return time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC) }

// newController gives each test its own pool inside the loopback filesystem, so
// they cannot see each other's volumes.
func newController(t *testing.T) (context.Context, *controller) {
	t.Helper()

	root := os.Getenv("BTRFS_TEST_POOL")
	if root == "" {
		t.Skip("BTRFS_TEST_POOL is unset; skipping tests that need a real btrfs filesystem")
	}

	pool := filepath.Join(root, "ctl-"+t.Name())
	if err := os.MkdirAll(pool, 0o755); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() { cleanupPool(t, pool) })

	return t.Context(), &controller{
		pool:        pool,
		nodeID:      "test-node",
		compression: "zstd",
		now:         testClock,
	}
}

// cleanupPool removes subvolumes with btrfs before deleting the tree; os.RemoveAll
// cannot remove a subvolume.
func cleanupPool(t *testing.T, pool string) {
	t.Helper()

	var subvolumes []string
	_ = filepath.WalkDir(pool, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == pool {
			return nil //nolint:nilerr // a partly-removed tree is not a test failure
		}
		if isSubvolume, _ := btrfs.IsSubvolume(path); isSubvolume {
			subvolumes = append(subvolumes, path)
		}
		return nil
	})

	slices.Reverse(subvolumes)
	for _, path := range subvolumes {
		if err := btrfs.DeleteSubvolume(context.Background(), path); err != nil {
			t.Errorf("cleanup %s: %v", path, err)
		}
	}
	if err := os.RemoveAll(pool); err != nil {
		t.Errorf("cleanup %s: %v", pool, err)
	}
}

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}
}

func deleteRequest(handle string) *csi.DeleteVolumeRequest {
	return &csi.DeleteVolumeRequest{VolumeId: handle}
}

func createRequest(claim, namespace, pvcName string, capacity int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               claim,
		CapacityRange:      &csi.CapacityRange{RequiredBytes: capacity},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
		Parameters: map[string]string{
			paramPVCNamespace: namespace,
			paramPVCName:      pvcName,
		},
	}
}

func TestCreateVolumeProvisionsSubvolumeWithQuota(t *testing.T) {
	ctx, c := newController(t)

	const capacity = 64 << 20
	resp, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", capacity))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if got, want := resp.GetVolume().GetVolumeId(), "localflix/library"; got != want {
		t.Errorf("volume id = %q, want %q", got, want)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != capacity {
		t.Errorf("capacity = %d, want %d", got, capacity)
	}
	if got := resp.GetVolume().GetAccessibleTopology(); len(got) != 1 || got[0].GetSegments()[TopologyNodeKey] != "test-node" {
		t.Errorf("accessible topology = %v, want a single segment pinning test-node", got)
	}

	path := filepath.Join(c.pool, "localflix", "library")
	if isSubvolume, err := btrfs.IsSubvolume(path); err != nil || !isSubvolume {
		t.Fatalf("IsSubvolume(%s) = %v, %v; want true", path, isSubvolume, err)
	}
	usage, err := btrfs.QuotaUsage(ctx, path)
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	if usage.Limit != capacity {
		t.Errorf("qgroup limit = %d, want %d", usage.Limit, capacity)
	}
	algorithm, err := btrfs.Compression(ctx, path)
	if err != nil {
		t.Fatalf("Compression: %v", err)
	}
	if algorithm != "zstd" {
		t.Errorf("compression = %q, want %q", algorithm, "zstd")
	}
}

func TestCreateVolumeIsIdempotent(t *testing.T) {
	ctx, c := newController(t)
	req := createRequest("pvc-abc", "localflix", "library", 32<<20)

	first, err := c.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	second, err := c.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("second CreateVolume: %v", err)
	}

	if first.GetVolume().GetVolumeId() != second.GetVolume().GetVolumeId() {
		t.Errorf("volume id changed between retries: %q then %q",
			first.GetVolume().GetVolumeId(), second.GetVolume().GetVolumeId())
	}
}

// The case human-readable names introduce: a PVC deleted under Retain and
// recreated resolves to a directory that still holds the old volume's data.
func TestCreateVolumeRefusesDirectoryHeldByAnotherClaim(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-first", "localflix", "library", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	_, err := c.CreateVolume(ctx, createRequest("pvc-second", "localflix", "library", 32<<20))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateVolume for a second claim = %v, want AlreadyExists", err)
	}
}

func TestCreateVolumeRefusesUnstampedDirectory(t *testing.T) {
	ctx, c := newController(t)

	stray := filepath.Join(c.pool, "localflix", "library")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateVolume over a foreign directory = %v, want AlreadyExists", err)
	}
}

// CSI is not Kubernetes-specific, so CreateVolume has to work from the request
// name alone. The volume lands under the fallback namespace, which is the
// visible signal that csi-provisioner is missing --extra-create-metadata.
func TestCreateVolumeWithoutPVCMetadataFallsBack(t *testing.T) {
	ctx, c := newController(t)

	req := createRequest("pvc-abc", "localflix", "library", 32<<20)
	req.Parameters = nil

	resp, err := c.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got, want := resp.GetVolume().GetVolumeId(), fallbackNamespace+"/pvc-abc"; got != want {
		t.Errorf("volume id = %q, want %q", got, want)
	}
}

// The same claim asked for at a different size is a conflict, not a retry.
func TestCreateVolumeRejectsCapacityChange(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	_, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 64<<20))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateVolume at a different capacity = %v, want AlreadyExists", err)
	}
}

func TestCreateVolumeRejectsBlockVolumes(t *testing.T) {
	ctx, c := newController(t)

	req := createRequest("pvc-abc", "localflix", "library", 32<<20)
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}

	_, err := c.CreateVolume(ctx, req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume for a block volume = %v, want InvalidArgument", err)
	}
}

// The CO only calls DeleteVolume for a PV whose reclaim policy is Delete, and
// takes success to mean the capacity is free again. Keeping the data under some
// other name would still hold its referenced bytes against the pool's qgroups,
// so the reply would be a lie. Retaining data is the reclaim policy's job, and
// under Retain this never runs at all.
func TestDeleteVolumeLeavesNothingBehind(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if _, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "localflix/library"}); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}

	if _, err := os.Stat(filepath.Join(c.pool, "localflix", "library")); !os.IsNotExist(err) {
		t.Errorf("volume still present after delete: %v", err)
	}

	// Walk the whole pool rather than just checking the original path: a
	// delete that quietly relocated the volume would pass the check above.
	var survivors []string
	err := filepath.WalkDir(c.pool, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != c.pool {
			if _, err := readClaim(path); err == nil {
				survivors = append(survivors, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk pool: %v", err)
	}
	if len(survivors) != 0 {
		t.Errorf("volumes survived the delete: %v", survivors)
	}
}

// The namespace directory has to outlive every volume but the last. Removing it
// while siblings remain would strand them; never removing it leaves an empty
// directory in the pool for every namespace that has ever held a volume.
func TestDeleteVolumeRemovesNamespaceDirectoryOnlyWhenEmpty(t *testing.T) {
	ctx, c := newController(t)

	for _, name := range []string{"library", "downloads"} {
		if _, err := c.CreateVolume(ctx, createRequest("pvc-"+name, "localflix", name, 32<<20)); err != nil {
			t.Fatalf("CreateVolume %s: %v", name, err)
		}
	}
	namespace := filepath.Join(c.pool, "localflix")

	if _, err := c.DeleteVolume(ctx, deleteRequest("localflix/library")); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := os.Stat(namespace); err != nil {
		t.Fatalf("namespace directory removed while a volume was still in it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(namespace, "downloads")); err != nil {
		t.Fatalf("surviving volume lost: %v", err)
	}

	if _, err := c.DeleteVolume(ctx, deleteRequest("localflix/downloads")); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if _, err := os.Stat(namespace); !os.IsNotExist(err) {
		t.Errorf("empty namespace directory survived the last volume: %v", err)
	}
}

func TestDeleteVolumeIsIdempotent(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "localflix/never-existed"}); err != nil {
		t.Fatalf("DeleteVolume on a missing volume = %v, want success", err)
	}
}

func TestDeleteVolumeRefusesPlainDirectory(t *testing.T) {
	ctx, c := newController(t)

	stray := filepath.Join(c.pool, "localflix", "library")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "localflix/library"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteVolume on a plain directory = %v, want FailedPrecondition", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("the directory should have been left alone: %v", err)
	}
}

// A traversing handle has to be a no-op rather than an error, because the spec
// requires DeleteVolume to succeed for volumes that do not exist. What matters
// is that nothing outside the pool is touched.
func TestDeleteVolumeIgnoresTraversingHandle(t *testing.T) {
	ctx, c := newController(t)

	outside := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	traversing, err := filepath.Rel(c.pool, outside)
	if err != nil {
		t.Fatalf("build traversing handle: %v", err)
	}

	if _, err := c.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: traversing}); err != nil {
		t.Fatalf("DeleteVolume with a traversing handle = %v, want success", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a file outside the pool was touched: %v", err)
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	resp, err := c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "localflix/library",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: %v", err)
	}
	if resp.GetConfirmed() == nil {
		t.Error("mount capability not confirmed")
	}

	_, err = c.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "localflix/missing",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ValidateVolumeCapabilities on a missing volume = %v, want NotFound", err)
	}
}
