#!/usr/bin/env bash
# Provisions a loopback btrfs filesystem with qgroups enabled, so tests can
# exercise real subvolume and quota behaviour instead of mocking it.
# Requires root.
set -euo pipefail

IMG=${BTRFS_TEST_IMG:-/tmp/btrfs-local-csi-test.img}
MNT=${BTRFS_TEST_MNT:-/mnt/btrfs-local-csi-test}
SIZE=${BTRFS_TEST_SIZE:-2G}

up() {
	if [ -e "$IMG" ]; then
		echo "$IMG already exists; run '$0 down' first" >&2
		exit 1
	fi
	truncate -s "$SIZE" "$IMG"
	mkfs.btrfs -q "$IMG"
	mkdir -p "$MNT"
	mount -o loop "$IMG" "$MNT"
	btrfs quota enable "$MNT"
	mkdir -p "$MNT/pool"
	# Tests run as an unprivileged user; only the btrfs ioctls need root.
	chmod 0777 "$MNT/pool"
	echo "pool ready at $MNT/pool"
}

down() {
	if mountpoint -q "$MNT"; then
		umount "$MNT"
	fi
	rm -f "$IMG"
	echo "cleaned up"
}

case "${1:-}" in
up) up ;;
down) down ;;
*)
	echo "usage: $0 {up|down}" >&2
	exit 1
	;;
esac
