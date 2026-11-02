#!/usr/bin/env bash
# Build the host's Mesa for virtio-gpu native context: radeonsi and RADV
# with the amdgpu virtio backend, and the libdrm that they need, into a
# prefix of their own. The system's Mesa stays as it is, and programs pick
# this one with scripts/mesa-env.sh. The libdrm is patched to accept the
# vDPA device that is the parent of virtio_gpu's DRM device.
#
# usage: mesa-build.sh [<prefix>]
#
# The default prefix is build/mesa. The sources go to build/libdrm-src and
# build/mesa-src, and the build trees to build/libdrm-build and
# build/mesa-build, or for another prefix to trees named after it. A prefix
# that the user cannot write, such as /opt/invirtio/mesa, is installed into
# a staging tree first and copied into place with sudo. Install Mesa's
# build dependencies first, with apt build-dep mesa.

set -euo pipefail

ROOT=$(realpath "$(dirname "$0")/..")
PREFIX=$(realpath -m "${1:-$ROOT/build/mesa}")
LIBDRM_TAG=libdrm-2.4.134
MESA_TAG=mesa-26.2.6

# Clone a source tree at a tag, unless it is there, and apply the patches,
# unless they are applied. A tree at another tag is an error.
fetch() {
    local url=$1 tag=$2 dir=$3 patch
    shift 3
    if [ ! -d "$dir" ]; then
        git clone --depth 1 --branch "$tag" "$url" "$dir"
    fi
    if [ "$(git -C "$dir" describe --tags --exact-match 2>/dev/null)" != "$tag" ]; then
        echo "${0##*/}: $dir is not at $tag, remove it to clone it again" >&2
        exit 1
    fi
    for patch in "$@"; do
        if ! git -C "$dir" apply --reverse --check "$patch" 2>/dev/null; then
            git -C "$dir" apply "$patch"
        fi
    done
}

# Another prefix gets build trees of its own, so that the default one keeps
# its configuration.
suffix=
if [ "$PREFIX" != "$ROOT/build/mesa" ]; then
    suffix=-$(echo "${PREFIX#/}" | tr / -)
fi
LIBDRM_BUILD=$ROOT/build/libdrm-build$suffix
MESA_BUILD=$ROOT/build/mesa-build$suffix

# A prefix that the user cannot write is staged, and replaces the prefix
# once it is built. Anything other than an earlier Mesa there is refused.
STAGE=
mkdir -p "$PREFIX" 2>/dev/null || true
if [ ! -w "$PREFIX" ]; then
    if [ -e "$PREFIX" ] && ! compgen -G "$PREFIX/lib/libgallium-*.so" >/dev/null; then
        echo "${0##*/}: $PREFIX exists and holds no Mesa, so it is not replaced" >&2
        exit 1
    fi
    STAGE=$ROOT/build/mesa-stage$suffix
    rm -rf "$STAGE"
    # The copy needs sudo, which asks now rather than after the build.
    sudo -v
fi

fetch https://gitlab.freedesktop.org/mesa/drm.git "$LIBDRM_TAG" \
    "$ROOT/build/libdrm-src" "$ROOT"/patches/libdrm/*.patch
fetch https://gitlab.freedesktop.org/mesa/mesa.git "$MESA_TAG" \
    "$ROOT/build/mesa-src"

meson setup --reconfigure "$LIBDRM_BUILD" "$ROOT/build/libdrm-src" \
    --prefix="$PREFIX" --libdir=lib --buildtype=release \
    -Damdgpu=enabled -Dradeon=disabled -Dintel=disabled -Dnouveau=disabled \
    -Dvmwgfx=disabled -Dman-pages=disabled -Dvalgrind=disabled \
    -Dcairo-tests=disabled -Dtests=false
meson install -C "$LIBDRM_BUILD" ${STAGE:+--destdir "$STAGE"}

# Mesa finds the new libdrm through pkg-config. A staged libdrm's files name
# their prefix relative to their own place, so that they work from the stage.
if [ -n "$STAGE" ]; then
    sed -i 's|^prefix=.*|prefix=${pcfiledir}/../..|' "$STAGE$PREFIX"/lib/pkgconfig/*.pc
fi
export PKG_CONFIG_PATH=$STAGE$PREFIX/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}
meson setup --reconfigure "$MESA_BUILD" "$ROOT/build/mesa-src" \
    --prefix="$PREFIX" --libdir=lib --buildtype=release \
    -Dgallium-drivers=radeonsi -Dvulkan-drivers=amd -Damdgpu-virtio=true \
    -Dllvm=enabled -Dglvnd=enabled -Dgallium-va=disabled \
    -Dgallium-rusticl=false \
    -Dvideo-codecs=
meson install -C "$MESA_BUILD" ${STAGE:+--destdir "$STAGE"}

# The copy belongs to root, since other users' sessions load it.
if [ -n "$STAGE" ]; then
    echo "${0##*/}: copying $STAGE$PREFIX to $PREFIX with sudo"
    sudo rm -rf "$PREFIX"
    sudo mkdir -p "$(dirname "$PREFIX")"
    sudo cp -dR --preserve=mode,timestamps "$STAGE$PREFIX" "$PREFIX"
fi
