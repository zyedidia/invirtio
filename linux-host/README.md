This is the Linux host tree for InvirtIO. This will be used if we make
modifications to the host kernel that runs InvirtIO drivers. At the moment this
is unused (the host kernel is still unpatched), but in the future we will use
this kernel to allow more virtio devices and features to be used from VDUSE.

Currently this tree contains two VDUSE patches. One allows virtio-gpu
devices, and the other falls back to an unbound workqueue when none of a
queue's affinity CPUs is online.

To install the kernel

```
make
sudo make modules_install
sudo make install
```
