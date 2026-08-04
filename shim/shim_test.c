// Exercises a built libqgroup_fence.so end to end: with the shim preloaded,
// does a fenced path report the quota, and does everything else still report
// the real filesystem?
//
// This has to be an external program rather than a unit test, because the
// shim reads its configuration in a constructor that runs before main — so
// the environment must already be set when the process starts. verify.sh
// drives it.
//
// Usage: shim_test fence|passthrough PATH QUOTA_BYTES AVAIL_BYTES

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/statvfs.h>
#include <sys/statfs.h>

static int failures;

static void check(const char *what, int want_fenced,
                  unsigned long long blocks, unsigned long long bavail,
                  unsigned long long bsize,
                  unsigned long long quota, unsigned long long avail) {
    int fenced = bsize > 0 && blocks == quota / bsize && bavail == avail / bsize;
    if (fenced == want_fenced) return;

    fprintf(stderr, "%s: expected the %s values, got blocks=%llu bavail=%llu bsize=%llu\n",
            what, want_fenced ? "fenced" : "real", blocks, bavail, bsize);
    failures++;
}

int main(int argc, char **argv) {
    if (argc != 5) {
        fprintf(stderr, "usage: %s fence|passthrough PATH QUOTA_BYTES AVAIL_BYTES\n", argv[0]);
        return 2;
    }

    int want_fenced = strcmp(argv[1], "fence") == 0;
    if (!want_fenced && strcmp(argv[1], "passthrough") != 0) {
        fprintf(stderr, "unknown mode %s\n", argv[1]);
        return 2;
    }

    const char *path = argv[2];
    unsigned long long quota = strtoull(argv[3], NULL, 10);
    unsigned long long avail = strtoull(argv[4], NULL, 10);

    struct statvfs vfs;
    if (statvfs(path, &vfs) != 0) {
        perror("statvfs");
        return 2;
    }
    check("statvfs", want_fenced, (unsigned long long)vfs.f_blocks,
          (unsigned long long)vfs.f_bavail, (unsigned long long)vfs.f_frsize, quota, avail);

    struct statfs fs;
    if (statfs(path, &fs) != 0) {
        perror("statfs");
        return 2;
    }
    check("statfs", want_fenced, (unsigned long long)fs.f_blocks,
          (unsigned long long)fs.f_bavail, (unsigned long long)fs.f_bsize, quota, avail);

#ifdef __GLIBC__
    struct statvfs64 vfs64;
    if (statvfs64(path, &vfs64) != 0) {
        perror("statvfs64");
        return 2;
    }
    check("statvfs64", want_fenced, (unsigned long long)vfs64.f_blocks,
          (unsigned long long)vfs64.f_bavail, (unsigned long long)vfs64.f_frsize, quota, avail);

    struct statfs64 fs64;
    if (statfs64(path, &fs64) != 0) {
        perror("statfs64");
        return 2;
    }
    check("statfs64", want_fenced, (unsigned long long)fs64.f_blocks,
          (unsigned long long)fs64.f_bavail, (unsigned long long)fs64.f_bsize, quota, avail);
#endif

    return failures ? 1 : 0;
}
