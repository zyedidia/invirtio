#!/bin/bash
# Run weston on the DRM card that the host's virtio_gpu registers for a
# guest's VDUSE device, with the Mesa that scripts/mesa-build.sh built. Run it
# from a text VT, where logind gives weston the VT and the input devices.
# Further arguments go to weston.
#
# usage: scripts/weston-gpu.sh [device name] [weston arguments]...

set -e
name=${1:-vgpu}
shift || true
dir=$(dirname "$(realpath "$0")")

cards=(/sys/bus/vdpa/devices/"$name"/drm/card*)
if [ ! -e "${cards[0]}" ]; then
	echo "weston-gpu.sh: the device $name has no DRM card, is its guest running and attached?" >&2
	exit 1
fi
. "$dir/mesa-env.sh"
exec weston --backend=drm --drm-device="$(basename "${cards[0]}")" "$@"
