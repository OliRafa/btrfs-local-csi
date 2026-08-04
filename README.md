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

### Quotas are invisible to `statvfs`

btrfs deliberately does not report qgroup limits through `statvfs`, so an
application inside a pod sees the whole pool's free space regardless of its
volume's quota. This is true of bind mounts and NFS alike, and there is no
upstream fix — the gap has been documented since 2016.

The driver therefore publishes each volume's qgroup numbers as JSON on the node,
for an `LD_PRELOAD` interposer to feed to applications that care.

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
the published quota state all work, and the upstream `csi-sanity` conformance
suite passes.

Still to come: the prebuilt `LD_PRELOAD` interposer image, and the deployment
manifests.

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
