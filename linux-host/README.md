This is the Linux host tree for InvirtIO. This will be used if we make
modifications to the host kernel that runs InvirtIO drivers. At the moment this
is unused (the host kernel is still unpatched), but in the future we will use
this kernel to allow more virtio devices and features to be used from VDUSE.

Currently this tree contains three VDUSE patches. Two allow virtio-gpu and
virtio-wlan devices, and the other falls back to an unbound workqueue when
none of a queue's affinity CPUs is online.

`modules/` builds host modules out of tree against the running kernel, so
that they load without booting this tree. It holds VDUSE from Debian's
6.12.107 (the same files as the stable `v6.12.107` tag) with the virtio-wlan
and workqueue patches backported. The module replaces the kernel's own:

```
make -C modules
sudo rmmod vduse
sudo insmod modules/vduse.ko
```

`sudo rmmod vduse && sudo modprobe vduse` returns to the kernel's.

`modules/` also builds the `virtio_wlan` driver from this tree, through
symbolic links to its source and headers. It loads with `sudo insmod
modules/virtio_wlan.ko` after `sudo modprobe cfg80211`.

To install the kernel

```
make
sudo make modules_install
sudo make install
```
