package driver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeClaims stands in for the Kubernetes API.
type fakeClaims struct {
	annotations map[string]string
	err         error
}

func (f fakeClaims) Annotations(context.Context, string, string) (map[string]string, error) {
	return f.annotations, f.err
}

func TestCreateVolumeHonoursNameAnnotation(t *testing.T) {
	ctx, c := newController(t, DeletionRename)
	c.claims = fakeClaims{annotations: map[string]string{NameAnnotation: "media"}}

	resp, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "localflix-library", 32<<20))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	if got, want := resp.GetVolume().GetVolumeId(), "localflix/media"; got != want {
		t.Errorf("volume id = %q, want %q", got, want)
	}
	if _, err := readClaim(filepath.Join(c.pool, "localflix", "media")); err != nil {
		t.Errorf("volume not created at the annotated name: %v", err)
	}
}

// Only the leaf is overridable, so a claim cannot reach into another namespace.
func TestCreateVolumeRejectsNameAnnotationEscape(t *testing.T) {
	ctx, c := newController(t, DeletionRename)
	c.claims = fakeClaims{annotations: map[string]string{NameAnnotation: "../pirate-bay/loot"}}

	_, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume with a traversing name annotation = %v, want InvalidArgument", err)
	}
}

func TestAnnotationsOverrideStorageClassOwnership(t *testing.T) {
	ctx, c := newController(t, DeletionRename)
	c.claims = fakeClaims{annotations: map[string]string{
		UIDAnnotation:  "3000",
		GIDAnnotation:  "3000",
		ModeAnnotation: "0750",
	}}

	// The class says 1000:1000 2770; the claim overrides all three.
	req := createRequest("pvc-abc", "bookstore", "comics-library", 32<<20)
	req.Parameters[paramUID] = "1000"
	req.Parameters[paramGID] = "1000"
	req.Parameters[paramMode] = "2770"

	if _, err := c.CreateVolume(ctx, req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(c.pool, "bookstore", "comics-library"), &st); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Uid != 3000 || st.Gid != 3000 {
		t.Errorf("owner = %d:%d, want 3000:3000", st.Uid, st.Gid)
	}
	if got := st.Mode & 0o7777; got != 0o750 {
		t.Errorf("mode = %o, want %o", got, 0o750)
	}
}

func TestStorageClassOwnershipAppliesWhenAnnotationsAreAbsent(t *testing.T) {
	ctx, c := newController(t, DeletionRename)
	c.claims = fakeClaims{annotations: map[string]string{}}

	req := createRequest("pvc-abc", "localflix", "library", 32<<20)
	req.Parameters[paramUID] = "1000"
	req.Parameters[paramGID] = "1000"

	if _, err := c.CreateVolume(ctx, req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(c.pool, "localflix", "library"), &st); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Uid != 1000 || st.Gid != 1000 {
		t.Errorf("owner = %d:%d, want the StorageClass default 1000:1000", st.Uid, st.Gid)
	}
}

// A failed lookup must not fall back to class defaults: the claim asked for a
// specific owner and provisioning it wrong leaves a volume the app cannot write.
func TestCreateVolumeFailsWhenAnnotationLookupFails(t *testing.T) {
	ctx, c := newController(t, DeletionRename)
	c.claims = fakeClaims{err: errors.New("api server unreachable")}

	_, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 32<<20))
	if status.Code(err) != codes.Internal {
		t.Fatalf("CreateVolume with a failing lookup = %v, want Internal", err)
	}
	var st unix.Stat_t
	if statErr := unix.Stat(filepath.Join(c.pool, "localflix", "library"), &st); statErr == nil {
		t.Error("a volume was created despite the lookup failing")
	}
}

func TestOwnershipParamsLayering(t *testing.T) {
	merged := ownershipParams(
		map[string]string{paramUID: "1000", paramGID: "1000", paramMode: "2770"},
		map[string]string{UIDAnnotation: "3000"},
	)

	if merged[paramUID] != "3000" {
		t.Errorf("uid = %q, want the annotation to win", merged[paramUID])
	}
	if merged[paramGID] != "1000" || merged[paramMode] != "2770" {
		t.Errorf("unannotated parameters should survive, got %v", merged)
	}
}
