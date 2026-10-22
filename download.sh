#!/bin/sh

set -eu

REPO=https://github.com/zyedidia/linux
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
	if [ -d "$1/linux" ]; then
		echo "$1/linux already exists, skipping"
		return
	fi
	git clone $DEPTH -b "$2" "$REPO" "$1/linux"
}

clone linux-uml  vkage-uml
clone linux-host vkage-host
