// LD_PRELOAD shim that overrides free-space reporting for configured path
// prefixes, sourcing replacement values from a JSON file written by the
// btrfs-local-csi driver's quota-state publisher.
//
// btrfs deliberately does not report qgroup limits through statvfs, so an
// application inside a pod sees the whole pool's free space however small its
// volume's quota is. There is no upstream fix; the gap has been documented
// since 2016. This is the workaround.
//
// Intercepted libc functions:
//   statvfs / statvfs64   POSIX (Python uses these)
//   statfs  / statfs64    Linux-native (.NET 10 uses statfs via
//                         HAVE_NON_LEGACY_STATFS; libtorrent likely too)
//
// statvfs64 and statfs64 are glibc-only extensions — on musl statvfs and
// statfs are already 64-bit and the *64 variants don't exist — so those
// paths are compiled in only when __GLIBC__ is defined.
//
// Env:
//   QGROUP_FENCE_PATHS  Colon-separated path prefixes to override.
//   QGROUP_FENCE_JSON   Path to {"quota_bytes":N,"avail_bytes":N}.
//
// The shim divides by the struct's block-size field at call time, so units
// are correct regardless of the underlying filesystem's block size (4 KiB
// on ext4, 1 MiB on NFS, etc.). For statvfs that's f_frsize; for statfs
// that's f_bsize.
//
// If the JSON is missing or malformed, the call returns unmodified pool
// stats — fail-open, matching the pre-shim behaviour.

#define _GNU_SOURCE
#include <sys/statvfs.h>
#include <sys/statfs.h>
#include <dlfcn.h>
#include <errno.h>
#include <string.h>
#include <stdio.h>
#include <stdlib.h>

static int (*real_statvfs)(const char *, struct statvfs *);
static int (*real_statfs)(const char *, struct statfs *);
#ifdef __GLIBC__
static int (*real_statvfs64)(const char *, struct statvfs64 *);
static int (*real_statfs64)(const char *, struct statfs64 *);
#endif
static const char *fence_paths;
static const char *fence_json;
static int initialized;

// Resolve the real libc entry points and read config. This MUST be safe to run
// lazily from inside an interposed call, not only from the constructor: an
// interposed function can fire before our constructor runs. On x86_64,
// libselinux's own initializer calls statfs64() during early startup of any
// binary linked against it (coreutils — id, ls, cp, …). If real_statfs64 is
// still NULL at that point the wrapper jumps through a NULL pointer and the
// process dies with SIGSEGV (latent on aarch64, where init order happens to run
// our constructor first). Resolving on demand removes the ordering dependency.
// Idempotent; the early path is single-threaded and the writes are pointer-
// sized, so no locking is needed.
static void shim_init(void) {
    if (initialized) return;
    real_statvfs = dlsym(RTLD_NEXT, "statvfs");
    real_statfs  = dlsym(RTLD_NEXT, "statfs");
#ifdef __GLIBC__
    real_statvfs64 = dlsym(RTLD_NEXT, "statvfs64");
    real_statfs64  = dlsym(RTLD_NEXT, "statfs64");
#endif
    fence_paths = getenv("QGROUP_FENCE_PATHS");
    fence_json = getenv("QGROUP_FENCE_JSON");
    initialized = 1;
}

__attribute__((constructor))
static void qgroup_fence_init(void) {
    shim_init();
}

// A prefix matches a whole path component, not a substring: fencing
// /data/downloads must not also fence /data/downloads-old, which is a
// different volume with a different quota.
static int path_matches(const char *path) {
    if (!path || !fence_paths || !*fence_paths) return 0;
    const char *p = fence_paths;
    while (*p) {
        const char *sep = strchr(p, ':');
        size_t len = sep ? (size_t)(sep - p) : strlen(p);
        // A trailing slash on the prefix would defeat the boundary check.
        while (len > 1 && p[len - 1] == '/') len--;
        if (len > 0 && strncmp(path, p, len) == 0 &&
            (path[len] == '\0' || path[len] == '/')) {
            return 1;
        }
        if (!sep) break;
        p = sep + 1;
    }
    return 0;
}

static int read_overlay(unsigned long *quota_bytes, unsigned long *avail_bytes) {
    if (!fence_json) return 0;
    FILE *f = fopen(fence_json, "r");
    if (!f) return 0;
    unsigned long q = 0, a = 0;
    int n = fscanf(f, " { \"quota_bytes\" : %lu , \"avail_bytes\" : %lu }", &q, &a);
    fclose(f);
    if (n != 2) return 0;
    *quota_bytes = q;
    *avail_bytes = a;
    return 1;
}

int statvfs(const char *path, struct statvfs *buf) {
    shim_init();
    if (!real_statvfs) { errno = ENOSYS; return -1; }
    int r = real_statvfs(path, buf);
    if (r == 0 && path_matches(path) && buf->f_frsize > 0) {
        unsigned long quota = 0, avail = 0;
        if (read_overlay(&quota, &avail)) {
            buf->f_blocks = quota / buf->f_frsize;
            buf->f_bfree = avail / buf->f_frsize;
            buf->f_bavail = avail / buf->f_frsize;
        }
    }
    return r;
}

int statfs(const char *path, struct statfs *buf) {
    shim_init();
    if (!real_statfs) { errno = ENOSYS; return -1; }
    int r = real_statfs(path, buf);
    if (r == 0 && path_matches(path) && buf->f_bsize > 0) {
        unsigned long quota = 0, avail = 0;
        if (read_overlay(&quota, &avail)) {
            buf->f_blocks = quota / buf->f_bsize;
            buf->f_bfree = avail / buf->f_bsize;
            buf->f_bavail = avail / buf->f_bsize;
        }
    }
    return r;
}

#ifdef __GLIBC__
int statvfs64(const char *path, struct statvfs64 *buf) {
    shim_init();
    if (!real_statvfs64) { errno = ENOSYS; return -1; }
    int r = real_statvfs64(path, buf);
    if (r == 0 && path_matches(path) && buf->f_frsize > 0) {
        unsigned long quota = 0, avail = 0;
        if (read_overlay(&quota, &avail)) {
            buf->f_blocks = quota / buf->f_frsize;
            buf->f_bfree = avail / buf->f_frsize;
            buf->f_bavail = avail / buf->f_frsize;
        }
    }
    return r;
}

int statfs64(const char *path, struct statfs64 *buf) {
    shim_init();
    if (!real_statfs64) { errno = ENOSYS; return -1; }
    int r = real_statfs64(path, buf);
    if (r == 0 && path_matches(path) && buf->f_bsize > 0) {
        unsigned long quota = 0, avail = 0;
        if (read_overlay(&quota, &avail)) {
            buf->f_blocks = quota / buf->f_bsize;
            buf->f_bfree = avail / buf->f_bsize;
            buf->f_bavail = avail / buf->f_bsize;
        }
    }
    return r;
}
#endif

// glibc version binding is handled by the build's --version-script=shim.ver:
//   GLIBC_2.2.5 { global: statvfs; statvfs64; statfs; statfs64; };
//
// That marks each export as @@GLIBC_2.2.5 (default version), satisfying
// both versioned callers (.NET 10 imports statfs64@GLIBC_2.2.5) and
// unversioned callers (Python, df). musl builds skip the version script
// entirely — symbol versioning isn't a thing on musl.
