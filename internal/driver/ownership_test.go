package driver

import (
	"path/filepath"
	"testing"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOwnershipFrom(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		want    ownership
		wantErr bool
	}{
		{name: "unset leaves everything alone", params: map[string]string{}},
		{
			name:   "uid, gid and mode",
			params: map[string]string{paramUID: "1000", paramGID: "1000", paramMode: "2770"},
			want:   ownership{uid: 1000, gid: 1000, mode: 0o2770, setOwner: true, setMode: true},
		},
		{
			name:   "mode alone",
			params: map[string]string{paramMode: "0750"},
			want:   ownership{mode: 0o750, setMode: true},
		},
		{name: "uid without gid", params: map[string]string{paramUID: "1000"}, wantErr: true},
		{name: "gid without uid", params: map[string]string{paramGID: "1000"}, wantErr: true},
		{name: "non-numeric uid", params: map[string]string{paramUID: "nobody", paramGID: "1000"}, wantErr: true},
		{name: "non-octal mode", params: map[string]string{paramMode: "0o999"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ownershipFrom(tc.params)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ownershipFrom(%v) = %+v, want an error", tc.params, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ownershipFrom: %v", err)
			}
			if got != tc.want {
				t.Errorf("ownershipFrom = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The setgid bit has to survive: a 2770 volume that comes out 0770 silently
// breaks group inheritance for files the application creates later.
func TestCreateVolumeAppliesOwnershipIncludingSetgid(t *testing.T) {
	ctx, c := newController(t)

	req := createRequest("pvc-abc", "localflix", "library", 32<<20)
	req.Parameters[paramUID] = "1000"
	req.Parameters[paramGID] = "1000"
	req.Parameters[paramMode] = "2770"

	if _, err := c.CreateVolume(ctx, req); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(c.pool, "localflix", "library"), &st); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Uid != 1000 || st.Gid != 1000 {
		t.Errorf("owner = %d:%d, want 1000:1000", st.Uid, st.Gid)
	}
	if got := st.Mode & 0o7777; got != 0o2770 {
		t.Errorf("mode = %o, want %o", got, 0o2770)
	}
}

func TestCreateVolumeRejectsBadOwnershipParameters(t *testing.T) {
	ctx, c := newController(t)

	req := createRequest("pvc-abc", "localflix", "library", 32<<20)
	req.Parameters[paramUID] = "1000" // no gid

	_, err := c.CreateVolume(ctx, req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("CreateVolume with a half-specified owner = %v, want InvalidArgument", err)
	}
}

func TestControllerExpandVolume(t *testing.T) {
	ctx, c := newController(t)

	const (
		initial = 32 << 20
		grown   = 96 << 20
	)
	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", initial)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	resp, err := c.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      "localflix/library",
		CapacityRange: &csi.CapacityRange{RequiredBytes: grown},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if resp.GetCapacityBytes() != grown {
		t.Errorf("capacity = %d, want %d", resp.GetCapacityBytes(), grown)
	}
	// The quota is the size, so there is nothing for the node to grow.
	if resp.GetNodeExpansionRequired() {
		t.Error("node expansion should not be required")
	}

	usage, err := btrfs.QuotaUsage(ctx, filepath.Join(c.pool, "localflix", "library"))
	if err != nil {
		t.Fatalf("QuotaUsage: %v", err)
	}
	if usage.Limit != grown {
		t.Errorf("qgroup limit = %d, want %d", usage.Limit, grown)
	}
}

func TestControllerExpandVolumeRefusesToShrink(t *testing.T) {
	ctx, c := newController(t)

	if _, err := c.CreateVolume(ctx, createRequest("pvc-abc", "localflix", "library", 64<<20)); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	_, err := c.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      "localflix/library",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 16 << 20},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("shrink = %v, want InvalidArgument", err)
	}
}

func TestControllerExpandVolumeRejectsMissingVolume(t *testing.T) {
	ctx, c := newController(t)

	_, err := c.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
		VolumeId:      "localflix/never-existed",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 16 << 20},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expand of a missing volume = %v, want NotFound", err)
	}
}
