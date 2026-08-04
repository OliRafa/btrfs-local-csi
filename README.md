# btrfs-local-csi

A CSI driver that provisions btrfs subvolumes with qgroup quotas and bind-mounts
them into pods. Built for a single-node home cluster where the Kubernetes node
*is* the NAS, so exporting storage over loopback NFS buys nothing and costs a
whole class of failures.

Replaces `btrfs-nfs-csi` on the `mountain-cloud` cluster.

## Why not local-path-provisioner

It cannot expand volumes. The `local` volume plugin does not implement
`ExpandableVolumePlugin`, so a PVC resize is admitted and then hangs in
`Resizing` forever with nothing to complete it. Its provisioning hook also only
receives `VOL_DIR`, `VOL_MODE` and `VOL_SIZE_BYTES` — no PVC name, namespace or
annotations — so per-volume ownership cannot be expressed.

## Design

| Aspect | Behaviour |
|---|---|
| Transport | `mount --bind`, no NFS |
| Layout | `<pool>/<namespace>/<pvc-name>`, leaf overridable per PVC |
| `volumeHandle` | The relative path itself, so Delete and Publish need no API read |
| Quota | btrfs qgroup `max_referenced` = requested size |
| Expansion | `btrfs qgroup limit`; online, no pod restart |
| Compression | `btrfs property set … compression zstd` |
| Ownership | uid/gid/mode from PVC annotations, StorageClass fallback |
| Stats | `NodeGetVolumeStats` reports qgroup usage, not pool usage |
| Deletion | `DeleteVolume` destroys the subvolume; retention is the reclaim policy's job |

`DeleteVolume` deliberately has no "move it aside instead" mode. The CO only
calls it for a PV whose reclaim policy is `Delete`, and reads success as "that
capacity is free now" — but a volume kept under some other name still holds
every referenced byte against the pool's qgroups, so the driver would be
reporting space it had not released. Under `Retain` the call never happens at
all, which is where keeping data belongs.

### Quotas are invisible to `statvfs`

btrfs deliberately does not report qgroup limits through `statvfs`, so an
application inside a pod sees the whole pool's free space regardless of its
volume's quota. This is true of bind mounts and NFS alike, and there is no
upstream fix — the gap has been documented since 2016.

The driver therefore publishes each volume's qgroup numbers as JSON on the node,
at `<quota-state-dir>/<namespace>/<name>.json`, and ships an `LD_PRELOAD`
interposer that answers `statvfs` and `statfs` from them.

### The interposer

`shim/` builds `ghcr.io/olirafa/btrfs-local-csi-shim`, holding a prebuilt
`libqgroup_fence.so` for each libc:

| | |
|---|---|
| `/preload/glibc/` | Built with `shim.ver`, exporting each symbol as `@@GLIBC_2.2.5`, because .NET imports `statfs64@GLIBC_2.2.5` and an unversioned export does not satisfy it |
| `/preload/musl/` | No version script, no `*64` entry points — musl has neither |

Pods copy the matching variant out of this image in an initContainer, set
`LD_PRELOAD`, `QGROUP_FENCE_PATHS` and `QGROUP_FENCE_JSON`, and mount the
driver's quota-state directory read-only. Prebuilding replaces an initContainer
that compiled the shim from source on every pod start.

Both variants are exercised against a real filesystem during the image build,
because every way this can break is silent — a shim that loads but intercepts
nothing just reports pool free space, which is the bug it exists to fix. The
build also asserts the glibc symbol versions are present and that the object
needs no glibc newer than 2.34.

The image is amd64 only: `GLIBC_2.2.5` is the x86_64 baseline and does not
exist on other architectures, where the exports would satisfy no import at all.

### Naming

A volume lives at `<pool>/<namespace>/<name>`, where `name` defaults to the PVC
name and can be overridden with the `btrfs-local-csi/name` annotation. Only the
leaf is configurable — the namespace prefix is always the claim's own, so a PVC
can never address another namespace's storage, and two same-named PVCs in
different namespaces cannot collide.

The handle *is* that relative path, because `DeleteVolume` and
`NodePublishVolume` have to locate a volume without reading the PVC, which by
deletion time no longer exists. The name is therefore fixed at provisioning:
editing the annotation later does nothing, since `volumeHandle` is immutable on
a bound PV.

## Status

Feature-complete for its first deployment. Provisioning, deletion, publishing,
online expansion, per-PVC ownership and name overrides, qgroup-backed stats and
the published quota state all work, the interposer ships prebuilt, and the
upstream `csi-sanity` conformance suite passes.

Still to come: the deployment manifests.

`csi-test` v5.5.0 does not build against csi spec v1.13.0, which dropped
`VOLUME_CONDITION`, so the spec is pinned to v1.12.0.

## Development

Requires Go 1.25, or Docker if you would rather not install it:

```sh
go test ./... -count=1
go build ./...
```

Tests covering subvolume and quota behaviour need a real btrfs filesystem:

```sh
sudo hack/btrfs-loop.sh up
sudo -E env "PATH=$PATH" BTRFS_TEST_POOL=/mnt/btrfs-local-csi-test/pool go test ./... -count=1
sudo hack/btrfs-loop.sh down
```
