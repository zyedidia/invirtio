#!/usr/bin/env bash
# Build dEQP's Vulkan conformance tests, deqp-vk, from VK-GL-CTS, to test the
# host's Mesa through virtio-gpu native context and natively.
#
# usage: deqp-build.sh
#
# The sources go to build/deqp-src, with the external sources that
# fetch_sources.py fetches, and the build tree to build/deqp-build. The
# binary is build/deqp-build/external/vulkancts/modules/vulkan/deqp-vk, next
# to the data that it reads. The build needs a C++ compiler, git, cmake,
# ninja, Python's lxml, and the GL headers that the Linux target builds
# against (apt install cmake ninja-build python3-lxml mesa-common-dev).

set -euo pipefail

ROOT=$(realpath "$(dirname "$0")/..")
SRC=$ROOT/build/deqp-src
OUT=$ROOT/build/deqp-build
TAG=vulkan-cts-1.4.7.1

if [ ! -d "$SRC" ]; then
    git clone --depth 1 --branch "$TAG" \
        https://github.com/KhronosGroup/VK-GL-CTS.git "$SRC"
fi
if [ "$(git -C "$SRC" describe --tags --exact-match 2>/dev/null)" != "$TAG" ]; then
    echo "${0##*/}: $SRC is not at $TAG, remove it to clone it again" >&2
    exit 1
fi
python3 "$SRC/external/fetch_sources.py"

cmake -S "$SRC" -B "$OUT" -G Ninja -DCMAKE_BUILD_TYPE=Release \
    -DSELECTED_BUILD_TARGETS=deqp-vk
cmake --build "$OUT"
