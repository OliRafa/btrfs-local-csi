package driver

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/OliRafa/btrfs-local-csi/internal/btrfs"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Parameters populated by csi-provisioner --extra-create-metadata. Without that
// flag the driver cannot tell which claim a volume belongs to, so it refuses to
// provision rather than guess.
const (
	paramPVCName      = "csi.storage.k8s.io/pvc/name"
	paramPVCNamespace = "csi.storage.k8s.io/pvc/namespace"
)

// TopologyNodeKey pins volumes to the node that holds the pool.
const TopologyNodeKey = "topology." + Name + "/node"

// ClaimLookup reads the annotations of a PersistentVolumeClaim. It is an
// interface so the driver can run outside a cluster, where it is simply nil.
type ClaimLookup interface {
	Annotations(ctx context.Context, namespace, name string) (map[string]string, error)
}

type controller struct {
	csi.UnimplementedControllerServer
	pool        string
	nodeID      string
	compression string
	claims      ClaimLookup
	now         func() time.Time
}

func (c *controller) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	claim := req.GetName()
	if claim == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	namespace := req.GetParameters()[paramPVCNamespace]
	pvcName := req.GetParameters()[paramPVCName]
	fromKubernetes := namespace != "" && pvcName != ""
	if !fromKubernetes {
		// CSI is not Kubernetes-specific, so CreateVolume has to work from the
		// request name alone. Under Kubernetes this branch means csi-provisioner
		// is missing --extra-create-metadata, and volumes land under the
		// generated pvc-<uuid> name rather than a readable one.
		slog.Warn("provisioning without PVC metadata; add --extra-create-metadata to csi-provisioner for readable paths",
			"name", claim)
		namespace, pvcName = fallbackNamespace, sanitizeSegment(claim)
	}

	// Annotations are the claim's own say over the name and ownership its
	// StorageClass would otherwise dictate.
	var (
		annotations map[string]string
		err         error
	)
	if fromKubernetes && c.claims != nil {
		annotations, err = c.claims.Annotations(ctx, namespace, pvcName)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
	}

	handle, err := ResolveHandle(namespace, pvcName, annotations[NameAnnotation])
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	path, err := VolumePath(c.pool, handle)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	own, err := ownershipFrom(ownershipParams(req.GetParameters(), annotations))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	capacity := requestedCapacity(req.GetCapacityRange())

	// Volume names are human-readable rather than UUIDs, so a directory may
	// already exist and belong to something else. The claim stamp is what tells
	// the two cases apart.
	switch existing, err := readClaim(path); {
	case err == nil && existing == claim:
		// A retry, unless the request changed size — the spec wants
		// ALREADY_EXISTS when the same name is asked for at a different
		// capacity.
		usage, err := btrfs.QuotaUsage(ctx, path)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
		if capacity > 0 && usage.Limit != capacity {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %q already exists with capacity %d, cannot satisfy a request for %d",
				handle, usage.Limit, capacity)
		}
		return createResponse(handle, usage.Limit, c.nodeID), nil
	case err == nil:
		return nil, status.Errorf(codes.AlreadyExists,
			"%s already holds volume %q, refusing to reuse it for %q", path, existing, claim)
	case errors.Is(err, errNoClaim):
		return nil, status.Errorf(codes.AlreadyExists,
			"%s already exists and was not created by this driver, refusing to adopt it", path)
	case errors.Is(err, os.ErrNotExist):
		// Fresh volume.
	default:
		return nil, status.Errorf(codes.Internal, "inspect %s: %v", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create namespace directory: %v", err)
	}
	if err := btrfs.CreateSubvolume(ctx, path); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	// Past this point a failure would leave a subvolume the provisioner never
	// learns about, so unwind it rather than leak.
	unwind := func(cause error) error {
		if err := btrfs.DeleteSubvolume(context.WithoutCancel(ctx), path); err != nil {
			slog.Error("could not unwind partially created volume", "path", path, "err", err)
		}
		return status.Errorf(codes.Internal, "%v", cause)
	}

	if capacity > 0 {
		if err := btrfs.SetQuota(ctx, path, capacity); err != nil {
			return nil, unwind(err)
		}
	} else {
		slog.Warn("provisioning without a quota; the volume can consume the whole pool", "volume", handle)
	}
	if c.compression != "" {
		if err := btrfs.SetCompression(ctx, path, c.compression); err != nil {
			return nil, unwind(err)
		}
	}
	if err := own.apply(path); err != nil {
		return nil, unwind(err)
	}
	if err := writeClaim(path, claim); err != nil {
		return nil, unwind(err)
	}

	slog.Info("created volume", "volume", handle, "path", path, "bytes", capacity)
	return createResponse(handle, capacity, c.nodeID), nil
}

func (c *controller) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	handle := req.GetVolumeId()
	if handle == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}

	// The spec requires DeleteVolume to succeed for a volume that does not
	// exist, and a handle that cannot even be parsed names a volume that cannot
	// exist. Returning OK here is safe precisely because it does nothing.
	path, err := ResolvedVolumePath(c.pool, handle)
	if err != nil {
		slog.Warn("delete requested for an unresolvable volume, ignoring", "volume", handle, "err", err)
		return &csi.DeleteVolumeResponse{}, nil
	}

	// The last guard before data goes away: whatever the handle resolved to has
	// to actually be one of our subvolumes.
	isSubvolume, err := btrfs.IsSubvolume(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if !isSubvolume {
		return nil, status.Errorf(codes.FailedPrecondition,
			"%s is not a btrfs subvolume, refusing to delete it", path)
	}

	// Destroying the subvolume is the whole contract: the CO only calls this for
	// a PV whose reclaim policy is Delete, and it takes the reply to mean the
	// capacity is free again. Keeping the data anyway — under a .trash prefix,
	// say — would still hold every referenced byte against the pool's qgroups,
	// so the driver would be reporting space it had not actually released.
	// Retaining data is the reclaim policy's job, and under Retain this method
	// is never called at all.
	if err := btrfs.DeleteSubvolume(ctx, path); err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}

	// The namespace directory is scaffolding the driver put there, not data, so
	// it goes when its last volume does — otherwise every namespace that ever
	// held a volume leaves an empty directory in the pool forever. Remove only
	// unlinks an empty directory, so this fails harmlessly while any sibling
	// volume remains, which is exactly when we want it to do nothing. A handle
	// is always <namespace>/<name>, so this can never reach the pool itself.
	_ = os.Remove(filepath.Dir(path))

	slog.Info("deleted volume", "volume", handle, "path", path)
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerExpandVolume moves the qgroup limit. There is no filesystem to
// grow, because the quota is the size, so expansion takes effect the moment the
// limit changes: no remount, no node-side step, no pod restart.
func (c *controller) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	handle := req.GetVolumeId()
	if handle == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	capacity := requestedCapacity(req.GetCapacityRange())
	if capacity <= 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}

	path, err := ResolvedVolumePath(c.pool, handle)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %q does not exist", handle)
	}

	usage, err := btrfs.QuotaUsage(ctx, path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	if usage.Limit > capacity {
		return nil, status.Errorf(codes.InvalidArgument,
			"cannot shrink volume %q from %d to %d", handle, usage.Limit, capacity)
	}
	if usage.Limit != capacity {
		if err := btrfs.SetQuota(ctx, path, capacity); err != nil {
			return nil, status.Errorf(codes.Internal, "%v", err)
		}
		slog.Info("expanded volume", "volume", handle, "from", usage.Limit, "to", capacity)
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         capacity,
		NodeExpansionRequired: false,
	}, nil
}

func (c *controller) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			rpcCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			rpcCapability(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		},
	}, nil
}

func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	handle := req.GetVolumeId()
	if handle == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	path, err := ResolvedVolumePath(c.pool, handle)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %q does not exist", handle)
	}
	if _, err := readClaim(path); err != nil {
		return nil, status.Errorf(codes.NotFound, "volume %q does not exist", handle)
	}

	if err := validateCapabilities(req.GetVolumeCapabilities()); err != nil {
		// An unsupported capability is a negative answer, not an error.
		return &csi.ValidateVolumeCapabilitiesResponse{}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

// validateCapabilities rejects raw block volumes: a btrfs subvolume is a
// directory, and there is nothing sensible to hand back as a block device.
func validateCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, c := range caps {
		if c.GetBlock() != nil {
			return status.Error(codes.InvalidArgument, "block volumes are not supported")
		}
		if c.GetMount() == nil {
			return status.Error(codes.InvalidArgument, "only mount volumes are supported")
		}
		switch c.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
			csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		default:
			return status.Errorf(codes.InvalidArgument, "unsupported access mode %s", c.GetAccessMode().GetMode())
		}
	}
	return nil
}

func requestedCapacity(r *csi.CapacityRange) int64 {
	if r == nil {
		return 0
	}
	if required := r.GetRequiredBytes(); required > 0 {
		return required
	}
	return r.GetLimitBytes()
}

func createResponse(handle string, capacity int64, nodeID string) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:           handle,
			CapacityBytes:      capacity,
			AccessibleTopology: []*csi.Topology{{Segments: map[string]string{TopologyNodeKey: nodeID}}},
		},
	}
}

func rpcCapability(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
		},
	}
}
