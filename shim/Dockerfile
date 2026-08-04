# Prebuilt LD_PRELOAD interposer that reports a volume's btrfs qgroup quota
# through statvfs/statfs, which btrfs itself will not do.
#
# Two variants, because the consumers differ: Torrentarr is glibc and imports
# statfs64@GLIBC_2.2.5, so it needs the version script; qBittorrent is musl,
# which has neither symbol versioning nor the *64 entry points. Shipping both
# prebuilt replaces an initContainer that compiled the shim on every pod start
# — one of which once sat hung for eight days waiting on apk behind a VPN
# kill-switch.
#
# Build amd64 only: shim.ver names GLIBC_2.2.5, which is the x86_64 baseline
# and does not exist on other architectures, where the shim would load and
# then silently do nothing.

FROM debian:12-slim AS glibc
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc libc6-dev binutils \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY shim.c shim.ver shim_test.c verify.sh ./

RUN install -d /preload/glibc \
 && gcc -shared -fPIC -O2 -Wall -Wextra -Werror \
      -Wl,--version-script=shim.ver \
      -o /preload/glibc/libqgroup_fence.so shim.c -ldl

# .NET imports statfs64@GLIBC_2.2.5, and an unversioned export does not satisfy
# it. The failure mode is silent — the app simply sees pool-level free space
# again — so assert the versions are there.
RUN for sym in statvfs statvfs64 statfs statfs64; do \
      objdump -T /preload/glibc/libqgroup_fence.so \
        | grep -qE "GLIBC_2\.2\.5[[:space:]]+${sym}\$" \
        || { echo "not exported as ${sym}@GLIBC_2.2.5"; exit 1; }; \
    done

# The shim is loaded into other people's containers, so it must not demand a
# newer glibc than they ship. The floor is set by dlsym, which moved into libc
# at 2.34; this pins that ceiling so a base-image bump cannot quietly raise it.
RUN required=$(objdump -T /preload/glibc/libqgroup_fence.so \
      | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1) \
 && echo "requires up to ${required}" \
 && [ "$(printf '%s\n%s\n' "$required" "GLIBC_2.34" | sort -V | tail -1)" = "GLIBC_2.34" ]

RUN gcc -O2 -Wall -Wextra -Werror -o /usr/local/bin/shim_test shim_test.c \
 && sh verify.sh /preload/glibc/libqgroup_fence.so /usr/local/bin/shim_test

FROM alpine:3.22 AS musl
RUN apk add --no-cache gcc musl-dev binutils
WORKDIR /src
COPY shim.c shim_test.c verify.sh ./

# No version script here: musl does not version symbols, and statvfs64 and
# statfs64 do not exist because statvfs and statfs are already 64-bit.
RUN install -d /preload/musl \
 && gcc -shared -fPIC -O2 -Wall -Wextra -Werror \
      -o /preload/musl/libqgroup_fence.so shim.c -ldl

RUN gcc -O2 -Wall -Wextra -Werror -o /usr/local/bin/shim_test shim_test.c \
 && sh verify.sh /preload/musl/libqgroup_fence.so /usr/local/bin/shim_test

# Consumed by an initContainer that copies the matching variant into a shared
# emptyDir, so all this image needs is the two objects and a cp.
FROM alpine:3.22
COPY --from=glibc /preload/glibc/libqgroup_fence.so /preload/glibc/libqgroup_fence.so
COPY --from=musl /preload/musl/libqgroup_fence.so /preload/musl/libqgroup_fence.so
