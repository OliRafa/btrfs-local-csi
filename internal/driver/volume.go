package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// NameAnnotation lets a PVC choose its on-disk directory name. Only the leaf is
// configurable: the namespace prefix is always the claim's own, so a PVC in one
// namespace can never address another namespace's storage.
const NameAnnotation = "btrfs-local-csi/name"

// trashDir holds volumes removed while the driver runs in rename mode. It is
// unreachable as a volume handle because the segment pattern rejects a leading
// dot.
const trashDir = ".trash"

// segmentPattern matches one path segment. It is the RFC 1123 shape Kubernetes
// already enforces on namespaces and PVC names, so default handles never need
// escaping — and it rejects "/", "." and ".." outright, which is what keeps a
// handle from escaping the pool.
var segmentPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

const maxSegmentLen = 253

// ResolveHandle builds the volume handle for a claim.
//
// The handle doubles as the volume's path relative to the pool. That is
// deliberate: DeleteVolume and NodePublishVolume must locate a volume without
// reading the PVC, which by deletion time no longer exists.
func ResolveHandle(namespace, pvcName, nameOverride string) (string, error) {
	if err := validateSegment("namespace", namespace); err != nil {
		return "", err
	}

	leaf := pvcName
	if nameOverride != "" {
		leaf = nameOverride
	}
	if err := validateSegment("volume name", leaf); err != nil {
		return "", err
	}

	return namespace + "/" + leaf, nil
}

func ValidateHandle(handle string) error {
	namespace, leaf, found := strings.Cut(handle, "/")
	if !found {
		return fmt.Errorf("volume handle %q must be <namespace>/<name>", handle)
	}
	if err := validateSegment("namespace", namespace); err != nil {
		return err
	}
	// A handle with extra segments leaves the slashes in leaf, which the
	// segment pattern rejects.
	return validateSegment("volume name", leaf)
}

// VolumePath resolves a handle to an absolute path inside the pool.
func VolumePath(pool, handle string) (string, error) {
	if err := ValidateHandle(handle); err != nil {
		return "", err
	}

	path := filepath.Join(pool, handle)
	if !isWithin(pool, path) {
		return "", fmt.Errorf("volume handle %q resolves outside the pool", handle)
	}
	return path, nil
}

// ResolvedVolumePath is VolumePath with symlinks resolved on both sides. Every
// operation that destroys data goes through this: a symlink planted inside the
// pool would otherwise point the driver at anything on the host.
func ResolvedVolumePath(pool, handle string) (string, error) {
	path, err := VolumePath(pool, handle)
	if err != nil {
		return "", err
	}

	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve volume path %q: %w", path, err)
	}
	realPool, err := filepath.EvalSymlinks(pool)
	if err != nil {
		return "", fmt.Errorf("resolve pool %q: %w", pool, err)
	}
	if !isWithin(realPool, realPath) {
		return "", fmt.Errorf("volume %q resolves to %q, which is outside the pool", handle, realPath)
	}
	return realPath, nil
}

// TrashPath is where a volume is moved when the driver deletes in rename mode.
func TrashPath(pool, handle string, at time.Time) string {
	name := strings.ReplaceAll(handle, "/", "-") + "-" + at.UTC().Format("20060102T150405Z")
	return filepath.Join(pool, trashDir, name)
}

// claimXattr records which CSI volume a directory belongs to.
const claimXattr = "user.btrfs-local-csi.claim"

// errNoClaim reports a directory that exists but carries no stamp, meaning this
// driver did not create it.
var errNoClaim = errors.New("directory carries no claim stamp")

// readClaim returns the CSI volume name stamped on a volume directory.
//
// The stamp is what makes human-readable paths safe. Names are no longer UUIDs,
// so a PVC deleted under a Retain policy and later recreated with the same name
// resolves to a directory that still holds the previous volume's data. Without
// the stamp the driver would silently hand it over.
func readClaim(path string) (string, error) {
	buf := make([]byte, 512)
	n, err := unix.Getxattr(path, claimXattr, buf)
	switch {
	case errors.Is(err, unix.ENOENT):
		return "", fmt.Errorf("stat %q: %w", path, os.ErrNotExist)
	case errors.Is(err, unix.ENODATA):
		return "", errNoClaim
	case err != nil:
		return "", fmt.Errorf("read claim stamp on %q: %w", path, err)
	}
	return string(buf[:n]), nil
}

func writeClaim(path, claim string) error {
	if err := unix.Setxattr(path, claimXattr, []byte(claim), 0); err != nil {
		return fmt.Errorf("stamp claim on %q: %w", path, err)
	}
	return nil
}

func validateSegment(kind, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", kind)
	case len(s) > maxSegmentLen:
		return fmt.Errorf("%s %q exceeds %d characters", kind, s, maxSegmentLen)
	case !segmentPattern.MatchString(s):
		return fmt.Errorf("%s %q must match %s", kind, s, segmentPattern)
	}
	return nil
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
