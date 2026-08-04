#!/bin/sh
# Verifies a freshly built libqgroup_fence.so against a real filesystem. Run
# inside each libc's build stage, so a shim that loads but silently does
# nothing fails the image build rather than production.
#
# Usage: verify.sh /path/to/libqgroup_fence.so /path/to/shim_test
set -eu

so=$1
test_bin=$2

# Deliberately not a round number: the checks compare against the real
# filesystem, and a plausible size could coincide with it.
quota=1234567168
avail=536870912

work=$(mktemp -d)
mkdir -p "$work/fenced/deeper" "$work/fenced-sibling" "$work/plain"

# Byte-for-byte what the driver's quota-state publisher writes.
printf '{"quota_bytes":%s,"avail_bytes":%s}' "$quota" "$avail" > "$work/data.json"
printf 'not json at all' > "$work/garbage.json"

run() {
	mode=$1
	path=$2
	json=$3
	QGROUP_FENCE_PATHS="$work/fenced" \
	QGROUP_FENCE_JSON="$json" \
	LD_PRELOAD="$so" \
		"$test_bin" "$mode" "$path" "$quota" "$avail"
}

run fence "$work/fenced" "$work/data.json"
run fence "$work/fenced/deeper" "$work/data.json"

run passthrough "$work/plain" "$work/data.json"
# A prefix must match whole components, or a sibling volume inherits the
# wrong quota.
run passthrough "$work/fenced-sibling" "$work/data.json"
# Fail open: no state yet, or a truncated read, must leave the real numbers
# alone rather than report zero free space.
run passthrough "$work/fenced" "$work/missing.json"
run passthrough "$work/fenced" "$work/garbage.json"

rm -rf "$work"
echo "verified $so"
