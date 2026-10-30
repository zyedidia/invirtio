#!/usr/bin/env bash
# Build the initramfs of a driver guest, with its userspace and firmware.

set -euo pipefail

PROG=${0##*/}

usage() {
    cat >&2 <<EOF
usage: $PROG [options] <build dir> <output dir>

Build an initramfs for the UML guest in <build dir>. It holds busybox, an
init script and the firmware that the guest's drivers load. The guest needs
no host filesystem to start or to load firmware.

The output directory gets three files:
  initramfs.list  a gen_init_cpio list, for CONFIG_INITRAMFS_SOURCE
  initramfs.cpio  the archive, for initrd=<file> or initrd=fd:<n>
  root/           generated files that the list refers to

Firmware options, which can be repeated:
  -d <module>  every MODULE_FIRMWARE file of a module built into the guest,
               such as mt7921e or cfg80211
  -l <log>     every file that a guest boot log reports as missing
  -f <name>    one file, named as the driver requests it

Other options:
  -i <script>  the init script, installed as /init. The default one mounts
               /dev, /proc and /sys and starts a shell
  -b <binary>  a host binary to add, with its shared libraries

Firmware is read from /lib/firmware/updates and /lib/firmware. Compressed
files are stored decompressed. regulatory.db and its signature come from the
-upstream files when the host has them, since the guest trusts only the
upstream signing key and Debian points the standard names at its own build. A driver often lists firmware for several
chips, so -d skips files that the host does not have. A guest that is still
missing firmware logs each file it could not load, and -l adds those files.
EOF
    exit 2
}

MODULES=()
LOGS=()
FIRMWARE=()
BINARIES=()
INIT=

while getopts d:l:f:i:b:h opt; do
    case $opt in
    d) MODULES+=("$OPTARG") ;;
    l) LOGS+=("$OPTARG") ;;
    f) FIRMWARE+=("$OPTARG") ;;
    i) INIT=$OPTARG ;;
    b) BINARIES+=("$OPTARG") ;;
    *) usage ;;
    esac
done
shift $((OPTIND - 1))
[ $# -eq 2 ] || usage
BUILD=$1
OUT=$2

die() {
    echo "$PROG: $*" >&2
    exit 1
}

warn() {
    echo "$PROG: $*" >&2
}

MODINFO=$BUILD/modules.builtin.modinfo
[ -f "$MODINFO" ] || die "$MODINFO does not exist, build the guest first"
[ -z "$INIT" ] || [ -f "$INIT" ] || die "$INIT does not exist"
BUSYBOX=$(command -v busybox) || die "busybox is not installed"

mkdir -p "$OUT"
OUT=$(realpath "$OUT")
ROOT=$OUT/root
rm -rf "$ROOT"
mkdir -p "$ROOT"
LIST=$OUT/initramfs.list
: > "$LIST"

declare -A HAVE

# Add a directory and its parents to the list.
add_dir() {
    local dir=$1
    [ "$dir" = / ] && return
    [ -n "${HAVE[$dir]:-}" ] && return
    add_dir "$(dirname "$dir")"
    HAVE[$dir]=1
    echo "dir $dir 0755 0 0" >> "$LIST"
}

# Add the host file <src> at <path> in the initramfs, unless <path> is
# already there.
add_file() {
    local path=$1 src=$2 mode=$3
    [ -n "${HAVE[$path]:-}" ] && return
    add_dir "$(dirname "$path")"
    HAVE[$path]=1
    echo "file $path $(realpath "$src") $mode 0 0" >> "$LIST"
}

add_link() {
    local path=$1 target=$2
    [ -n "${HAVE[$path]:-}" ] && return
    add_dir "$(dirname "$path")"
    HAVE[$path]=1
    echo "slink $path $target 0777 0 0" >> "$LIST"
}

# Add a binary and the shared libraries it loads. glibc also opens libgcc_s
# at run time to unwind threads in pthread_cancel(), which ldd does not list.
add_binary() {
    local bin=$1 lib gcc_s
    add_file "$bin" "$bin" 0755
    for lib in $(ldd "$bin" 2>/dev/null |
                 awk '$2 == "=>" && $3 ~ /^\// { print $3 } $1 ~ /^\// { print $1 }'); do
        add_file "$lib" "$lib" 0755
        case $lib in
        */libc.so.6)
            gcc_s=$(dirname "$lib")/libgcc_s.so.1
            [ ! -f "$gcc_s" ] || add_file "$gcc_s" "$gcc_s" 0755
            ;;
        esac
    done
}

FW_DIRS=(/lib/firmware/updates /lib/firmware)
FW_BYTES=0
FW_COUNT=0

# Add the firmware file <name>. Return 1 if the host does not have it.
add_firmware() {
    local name=$1 dir src dst upstream=
    [ -n "${HAVE[/lib/firmware/$name]:-}" ] && return 0
    case $name in
    regulatory.db | regulatory.db.p7s) upstream=$name-upstream ;;
    esac
    for dir in "${FW_DIRS[@]}"; do
        if [ -n "$upstream" ] && [ -f "$dir/$upstream" ]; then
            src=$dir/$upstream
        elif [ -f "$dir/$name" ]; then
            src=$dir/$name
        elif [ -f "$dir/$name.zst" ]; then
            dst=$ROOT/lib/firmware/$name
            mkdir -p "$(dirname "$dst")"
            zstd -q -d -c "$dir/$name.zst" > "$dst" ||
                die "could not decompress $dir/$name.zst"
            src=$dst
        elif [ -f "$dir/$name.xz" ]; then
            dst=$ROOT/lib/firmware/$name
            mkdir -p "$(dirname "$dst")"
            xz -d -c "$dir/$name.xz" > "$dst" ||
                die "could not decompress $dir/$name.xz"
            src=$dst
        else
            continue
        fi
        add_file "/lib/firmware/$name" "$src" 0644
        FW_BYTES=$((FW_BYTES + $(stat -L -c %s "$src")))
        FW_COUNT=$((FW_COUNT + 1))
        return 0
    done
    return 1
}

add_dir /dev
echo "nod /dev/console 0600 0 0 c 5 1" >> "$LIST"
add_dir /proc
add_dir /sys
add_dir /tmp

if [ -z "$INIT" ]; then
    INIT=$ROOT/init
    cat > "$INIT" <<'EOF'
#!/bin/sh
export PATH=/sbin:/usr/sbin:/bin:/usr/bin
mount -t devtmpfs devtmpfs /dev
mount -t proc proc /proc
mount -t sysfs sysfs /sys
exec sh
EOF
fi
add_file /init "$INIT" 0755

for bin in "${BINARIES[@]}"; do
    [ -f "$bin" ] || die "$bin does not exist"
    add_binary "$(realpath -s "$bin")"
done

add_binary "$BUSYBOX"
for applet in $("$BUSYBOX" --list-full); do
    add_link "/$applet" "$BUSYBOX"
done

for mod in "${MODULES[@]}"; do
    names=$(tr '\0' '\n' < "$MODINFO" | sed -n "s/^$mod\.firmware=//p")
    [ -n "$names" ] || warn "module $mod has no MODULE_FIRMWARE entries in $BUILD"
    for name in $names; do
        add_firmware "$name" || warn "skipping $name, which is not on the host"
    done
done

for log in "${LOGS[@]}"; do
    [ -f "$log" ] || die "$log does not exist"
    for name in $(sed -n 's/.*Direct firmware load for \([^ ]*\) failed with error.*/\1/p' "$log" |
                  sort -u); do
        add_firmware "$name" || warn "$name is not on the host"
    done
done

for name in "${FIRMWARE[@]}"; do
    add_firmware "$name" || die "$name is not on the host"
done

GEN=$BUILD/usr/gen_init_cpio
[ -x "$GEN" ] || die "$GEN does not exist, build the guest with CONFIG_BLK_DEV_INITRD"
"$GEN" "$LIST" > "$OUT/initramfs.cpio"

echo "$PROG: $FW_COUNT firmware files ($((FW_BYTES / 1024)) KiB)," \
     "archive $(( $(stat -c %s "$OUT/initramfs.cpio") / 1024 )) KiB" >&2
