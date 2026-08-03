package driver

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type node struct {
	csi.UnimplementedNodeServer
	pool   string
	nodeID string
}

func (n *node) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	handle := req.GetVolumeId()
	target := req.GetTargetPath()
	switch {
	case handle == "":
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	case target == "":
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	case req.GetVolumeCapability() == nil:
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if err := validateCapabilities([]*csi.VolumeCapability{req.GetVolumeCapability()}); err != nil {
		return nil, err
	}

	source, err := ResolvedVolumePath(n.pool, handle)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, status.Errorf(codes.NotFound, "volume %q does not exist", handle)
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	isSubvolume, err := btrfs.IsSubvolume(source)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !isSubvolume {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is not a btrfs subvolume", source)
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create target %s: %v", target, err)
	}

	mounted, err := isMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if mounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount %s at %s: %v", source, target, err)
	}

	if req.GetReadonly() {
		// MS_RDONLY is ignored on the initial MS_BIND; making a bind mount
		// read-only takes a second, remounting call.
		if err := unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			if uerr := unix.Unmount(target, unix.MNT_DETACH); uerr != nil {
				slog.Error("could not unwind bind mount", "target", target, "err", uerr)
			}
			return nil, status.Errorf(codes.Internal, "remount %s read-only: %v", target, err)
		}
	}

	slog.Info("published volume", "volume", handle, "target", target, "readonly", req.GetReadonly())
	return &csi.NodePublishVolumeResponse{}, nil
}

func (n *node) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	mounted, err := isMountPoint(target)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if mounted {
		if err := unix.Unmount(target, 0); err != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", target, err)
		}
	}

	// Only ever removes the empty directory left behind by the bind mount; if
	// anything is still there, leave it rather than delete data.
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		slog.Warn("could not remove target directory", "target", target, "err", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (n *node) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	handle := req.GetVolumeId()
	volumePath := req.GetVolumePath()
	if handle == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	if _, err := os.Stat(volumePath); err != nil {
		return nil, status.Errorf(codes.NotFound, "volume path %s: %v", volumePath, err)
	}

	source, err := ResolvedVolumePath(n.pool, handle)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, status.Errorf(codes.NotFound, "volume %q does not exist", handle)
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	usage, err := btrfs.QuotaUsage(ctx, source)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	// Reporting the qgroup rather than the filesystem is the whole point:
	// statvfs shows the entire pool, so without this every volume looks as
	// large as the disk.
	total, used, available := usage.Limit, usage.Referenced, usage.Available()
	if usage.Limit == 0 {
		var fs unix.Statfs_t
		if err := unix.Statfs(source, &fs); err != nil {
			return nil, status.Errorf(codes.Internal, "statfs %s: %v", source, err)
		}
		block := int64(fs.Bsize)
		total = int64(fs.Blocks) * block
		available = int64(fs.Bavail) * block
		used = total - int64(fs.Bfree)*block
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{{
			Unit:      csi.VolumeUsage_BYTES,
			Total:     total,
			Used:      used,
			Available: available,
		}},
	}, nil
}

func (n *node) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			nodeCapability(csi.NodeServiceCapability_RPC_GET_VOLUME_STATS),
		},
	}, nil
}

func (n *node) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId:             n.nodeID,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{TopologyNodeKey: n.nodeID}},
	}, nil
}

func nodeCapability(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: t},
		},
	}
}

// isMountPoint reports whether path currently has something mounted on it.
func isMountPoint(path string) (bool, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// The mount point is the fifth field: id, parent, dev, root, mountpoint.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if unescapeMountPoint(fields[4]) == resolved {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// unescapeMountPoint reverses the octal escaping the kernel applies to
// characters that would otherwise break mountinfo's field separation.
var mountPointUnescaper = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func unescapeMountPoint(s string) string { return mountPointUnescaper.Replace(s) }
