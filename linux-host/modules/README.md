# Out-of-tree builds of the linux-host modules

The host-side changes carried by `linux-host/linux` (commits
`0c049060c1e7` "vduse: allow CTRL_VQ with timeout" and `4234b360645b`
"virtio: add virtio-wlan device" on top of 7.2-rc6), extracted so they
can be built against a stock distro kernel's headers instead of
installing the custom host kernel:

```
make            # builds vduse/vduse.ko and virtio_wlan/virtio_wlan.ko
                # against /lib/modules/$(uname -r)/build; override with KDIR=
```

* `vduse/` — stable v6.17 vduse sources with the vkage patch applied:
  WLAN device id allowlisted, `VIRTIO_NET_F_CTRL_VQ` allowed, and the
  `VDUSE_VQ_INJECT_IRQ2` inline-injection ioctl. The patched uapi header
  is carried as a local copy (`vduse.h`). The 6.17 base predates the
  IOTLB v2/asid work, so `VDUSE_IOTLB_GET_FD2` (0x1b) does not exist
  here; `VDUSE_VQ_INJECT_IRQ2` stays at 0x1c to match the 7.2 tree.
  Only here, not yet in `linux-host/linux`: virtio-input is allowlisted
  (needs `CAP_SYS_ADMIN`), and its config-space writes are relayed to
  userspace as a `VDUSE_SET_CONFIG` (0x100) request, since the host's
  virtio_input driver selects what to read by writing config space.
* `virtio_wlan/` — the new driver plus a local copy of its uapi header
  (`virtio_wlan.h`). Backport delta vs the in-tree 7.2 copy: on 6.17 the
  `add_key`, `del_key`, `set_default_mgmt_key`, `get_station` and
  `change_station` cfg80211 ops take `struct net_device *` rather than
  `struct wireless_dev *` (none of the bodies use the argument).

Not extracted: the third change in the linux-host tree, a 12-line
control-command timeout in `drivers/net/virtio_net.c`. It matters only
when exporting a *net* device with `VIRTIO_NET_F_CTRL_VQ` (an unpatched
virtio_net spins forever on a control command a dead UML never answers);
the WiFi path does not use virtio_net. Rebuild virtio_net.ko the same
way if you need it.

On a Secure Boot host (lockdown), sign before loading:

```
/usr/src/linux-headers-$(uname -r)/scripts/sign-file sha512 MOK.priv MOK.der <module>.ko
```

(one-time key enrollment: `mokutil --import MOK.der`, then reboot and
confirm in MokManager).

Load order for the WiFi example — the stock vduse module must not be
loaded, since these replace it:

```
sudo modprobe -a vhost_iotlb vdpa vfio-pci
sudo insmod modules/vduse/vduse.ko
sudo modprobe virtio_vdpa
sudo insmod modules/virtio_wlan/virtio_wlan.ko
```
