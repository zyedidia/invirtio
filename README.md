# InvirtIO

InvirtIO is a system for creating virtio devices that are backed by real Linux
device drivers running in userspace and controlling the device.

* linux-uml holds the source code for the UML guest. There are significant
  modifications here.
* linux-host holds the source code for the host kernel. Most modifications here
  are contained to kernel modules so they can be reloaded without rebooting the
  host kernel (after installing a custom kernel in the host).

Run `./download.sh --shallow` to download the two Linux kernels and the Cloud
Hypervisor fork used by the KVM prototype (`docs/KVM.md`).
