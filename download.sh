#!/bin/sh

set -eu

LINUX=https://github.com/zyedidia/linux
CH=https://github.com/zyedidia/cloud-hypervisor
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
