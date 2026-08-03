// Package btrfs wraps the btrfs-progs CLI. The driver shells out rather than
// issuing ioctls directly: btrfs-progs is a stable, debuggable interface, and
// every operation here happens at most once per volume lifecycle event, so
// process overhead is irrelevant.
package btrfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// binary is a variable so tests can point at a stub.
var binary = "btrfs"

// subvolumeRootInode is BTRFS_FIRST_FREE_OBJECTID: the root directory of every
// btrfs subvolume has this inode number, and no other directory does.
const subvolumeRootInode = 256

// Usage reports a subvolume's qgroup accounting, in bytes.
type Usage struct {
	Referenced int64
	// Limit is max_referenced, or 0 when no limit is set.
	Limit int64
}

// Available returns the bytes left under the quota, clamped at zero.
//
// Clamping matters: a subvolume can end up over its limit (the limit was
// lowered, or accounting caught up after the fact), and a negative value fed
// into the unsigned block counts of statvfs wraps into an enormous number,
// telling applications they have terabytes free.
func (u Usage) Available() int64 {
	if u.Limit <= 0 {
		return 0
	}
	if u.Referenced >= u.Limit {
		return 0
	}
	return u.Limit - u.Referenced
}

func CreateSubvolume(ctx context.Context, path string) error {
	_, err := run(ctx, "subvolume", "create", path)
	return err
}

func DeleteSubvolume(ctx context.Context, path string) error {
	_, err := run(ctx, "subvolume", "delete", path)
	return err
}

// IsSubvolume reports whether path is the root of a btrfs subvolume. It checks
// the filesystem type as well as the inode, because inode 256 is only
// meaningful on btrfs.
func IsSubvolume(path string) (bool, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return false, fmt.Errorf("statfs %q: %w", path, err)
	}
	if fs.Type != unix.BTRFS_SUPER_MAGIC {
		return false, nil
	}

	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return false, fmt.Errorf("lstat %q: %w", path, err)
	}
	return st.Ino == subvolumeRootInode, nil
}

// SetQuota sets the qgroup max_referenced limit. A limit of zero or less
// removes the limit.
func SetQuota(ctx context.Context, path string, bytes int64) error {
	limit := "none"
	if bytes > 0 {
		limit = strconv.FormatInt(bytes, 10)
	}
	_, err := run(ctx, "qgroup", "limit", limit, path)
	return err
}

func QuotaUsage(ctx context.Context, path string) (Usage, error) {
	out, err := run(ctx, "--format=json", "qgroup", "show", "-re", "--raw", "-f", path)
	if err != nil {
		return Usage{}, err
	}

	var parsed struct {
		Qgroups []struct {
			Referenced    int64     `json:"referenced"`
			MaxReferenced noneInt64 `json:"max_referenced"`
		} `json:"qgroup-show"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		return Usage{}, fmt.Errorf("parse qgroup output for %q: %w", path, err)
	}
	if len(parsed.Qgroups) == 0 {
		return Usage{}, fmt.Errorf("no qgroup found for %q; are quotas enabled on this filesystem?", path)
	}

	q := parsed.Qgroups[0]
	return Usage{Referenced: q.Referenced, Limit: int64(q.MaxReferenced)}, nil
}

func SetCompression(ctx context.Context, path, algorithm string) error {
	_, err := run(ctx, "property", "set", path, "compression", algorithm)
	return err
}

func Compression(ctx context.Context, path string) (string, error) {
	out, err := run(ctx, "property", "get", path, "compression")
	if err != nil {
		return "", err
	}
	// Output is "compression=zstd", or empty when unset.
	_, value, found := strings.Cut(strings.TrimSpace(out), "=")
	if !found {
		return "", nil
	}
	return value, nil
}

// Sync flushes pending metadata so qgroup accounting reflects recent writes.
func Sync(ctx context.Context, path string) error {
	_, err := run(ctx, "filesystem", "sync", path)
	return err
}

// noneInt64 decodes a btrfs JSON field that is a number when a limit is set and
// the string "none" when it is not.
type noneInt64 int64

func (n *noneInt64) UnmarshalJSON(b []byte) error {
	if string(b) == `"none"` {
		*n = 0
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*n = noneInt64(v)
	return nil
}

func run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("btrfs %s: %s", strings.Join(args, " "), msg)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("btrfs %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("run btrfs %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
