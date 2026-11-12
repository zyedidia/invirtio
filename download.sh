#!/bin/sh

set -eu

LINUX=https://github.com/zyedidia/linux
CH=https://github.com/zyedidia/cloud-hypervisor
FIRMWARE=https://gitlab.com/kernel-firmware/linux-firmware.git
FIRMWARE_TAG=20261019
DEPTH=

usage() {
	echo "usage: ${0##*/} [--shallow]" >&2
	exit 1
}

for arg; do
	case $arg in
	--shallow) DEPTH="--depth 1" ;;
	-h|--help)  usage ;;
	*) echo "${0##*/}: unknown option: $arg" >&2; usage ;;
	esac
done

clone() {
	if [ -d "$1" ]; then
		echo "$1 already exists, skipping"
		return
	fi
	git clone $DEPTH -b "$3" "$2" "$1"
}

clone linux-uml/linux       "$LINUX" invirtio-uml
clone linux-host/linux      "$LINUX" invirtio-host
clone kvm/cloud-hypervisor  "$CH"    invirtio

# The GPU guests take their firmware from a release of linux-firmware. A
# partial clone fetches only the amdgpu files and their license, during the
# checkout, which a rerun repeats until it succeeds.
if [ -d linux-firmware/amdgpu ]; then
	echo "linux-firmware already exists, skipping"
else
	[ -d linux-firmware ] ||
		git clone --filter=blob:none --no-checkout --depth 1 \
			-b "$FIRMWARE_TAG" "$FIRMWARE" linux-firmware
	git -C linux-firmware sparse-checkout set --no-cone /amdgpu/ /WHENCE \
		/LICENSES/LICENSE.amdgpu
	git -C linux-firmware checkout -q "$FIRMWARE_TAG"
fi
