This is the Linux host tree for InvirtIO. The host runs this kernel, which
allows more virtio devices and features to be used from VDUSE.

Currently this tree contains three VDUSE patches. Two allow virtio-gpu and
virtio-wlan devices, and the other falls back to an unbound workqueue when
none of a queue's affinity CPUs is online. It also adds the `virtio_wlan`
host driver.

The kernel is built in `build/` from the Debian kernel's configuration, with
`vdpa`, `vduse`, `virtio_vdpa` and `virtio_wlan` as modules and without debug
information:

```
cd linux
mkdir -p ../build
cp /boot/config-$(uname -r) ../build/.config
scripts/config --file ../build/.config \
    --set-str SYSTEM_TRUSTED_KEYS "" --set-str SYSTEM_REVOCATION_KEYS "" \
    --disable DEBUG_INFO_DWARF5 --disable DEBUG_INFO_DWARF4 \
    --disable DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT --enable DEBUG_INFO_NONE \
    --set-str LOCALVERSION "-invirtio" \
    --module VDPA --module VDPA_USER --module VIRTIO_VDPA --module VIRTIO_WLAN
make O=../build olddefconfig
make O=../build -j$(nproc)
sudo make O=../build modules_install
sudo make O=../build install
```

After a change to a module, `make O=../build modules_install` and reloading
the module is enough, without a reboot.

The modules are unsigned, so Secure Boot must be off.
