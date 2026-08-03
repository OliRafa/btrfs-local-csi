package driver

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// StorageClass parameters controlling the ownership of a volume's root
// directory. Applications run as a fixed uid and cannot chown a directory they
// do not own, so the driver has to get this right at creation time.
const (
	paramUID  = "uid"
	paramGID  = "gid"
	paramMode = "mode"
)

// PVC annotations overriding the StorageClass parameters above.
const (
	UIDAnnotation  = "btrfs-local-csi/uid"
	GIDAnnotation  = "btrfs-local-csi/gid"
	ModeAnnotation = "btrfs-local-csi/mode"
)

// ownershipParams layers a claim's annotations over its StorageClass
// parameters, so a PVC can opt out of the class-wide default without needing a
// StorageClass of its own.
func ownershipParams(params, annotations map[string]string) map[string]string {
	merged := make(map[string]string, 3)
	for _, key := range []string{paramUID, paramGID, paramMode} {
		if value, ok := params[key]; ok {
			merged[key] = value
		}
	}
	for annotation, key := range map[string]string{
		UIDAnnotation:  paramUID,
		GIDAnnotation:  paramGID,
		ModeAnnotation: paramMode,
	} {
		if value, ok := annotations[annotation]; ok {
			merged[key] = value
		}
	}
	return merged
}

// ownership is unset by default, leaving whatever btrfs created.
type ownership struct {
	uid, gid int
	mode     uint32
	setOwner bool
	setMode  bool
}

func ownershipFrom(params map[string]string) (ownership, error) {
	var own ownership

	uid, hasUID := params[paramUID]
	gid, hasGID := params[paramGID]
	if hasUID != hasGID {
		return ownership{}, fmt.Errorf("parameters %q and %q must be set together", paramUID, paramGID)
	}
	if hasUID {
		var err error
		if own.uid, err = strconv.Atoi(uid); err != nil {
			return ownership{}, fmt.Errorf("parameter %q: %w", paramUID, err)
		}
		if own.gid, err = strconv.Atoi(gid); err != nil {
			return ownership{}, fmt.Errorf("parameter %q: %w", paramGID, err)
		}
		own.setOwner = true
	}

	if mode, ok := params[paramMode]; ok {
		// Octal, and parsed as raw bits rather than through os.FileMode so that
		// setgid survives: Go remaps those bits, and a 2770 volume that comes
		// out 0770 silently breaks group inheritance for new files.
		parsed, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return ownership{}, fmt.Errorf("parameter %q must be octal: %w", paramMode, err)
		}
		own.mode = uint32(parsed)
		own.setMode = true
	}

	return own, nil
}

func (o ownership) apply(path string) error {
	if o.setOwner {
		if err := unix.Chown(path, o.uid, o.gid); err != nil {
			return fmt.Errorf("chown %q to %d:%d: %w", path, o.uid, o.gid, err)
		}
	}
	if o.setMode {
		if err := unix.Chmod(path, o.mode); err != nil {
			return fmt.Errorf("chmod %q to %o: %w", path, o.mode, err)
		}
	}
	return nil
}
