# invirtio

`invirtio` installs InvirtIO and manages its driver guests. Each enabled
device gets a guest of its own, which runs the device's real driver under
the sandboxed launcher and exports the device to the host as a virtio-wlan
or virtio-net device.

All guests run one UML build that has every supported driver and both
backends. `invirtio` picks the backend from the device's PCI class, and the
guest's kernel binds the driver that matches the device.

## Building

```
make -C launcher
make -C tools
scripts/invirtio-build-guest.sh
```

The host must run the `linux-host` kernel, with the IOMMU on and the
`vduse`, `vdpa`, `virtio_vdpa` and `virtio_wlan` modules (see
`linux-host/README.md`), and it needs the `vdpa` command from iproute2.

## Use

```
sudo build/invirtio install -guest linux-uml/build-invirtio
invirtio list
sudo invirtio enable 06:00.0
invirtio status
sudo invirtio console wlan-06-00-0
sudo invirtio disable wlan-06-00-0
sudo invirtio uninstall
```

`install`, `enable`, `disable` and `uninstall` run in stages. Each stage
prints the shell commands that do the same as its steps, and waits for
Enter before it runs them. With `-n`, they print every stage and change
nothing, which needs no root, and with `-y`, they do not wait.

- `install` copies the guest, its initramfs, the launcher and `invirtio`
  to `/usr/lib/invirtio` and `/usr/local/sbin`, adds the `invirtio` user
  that the guests run as, and installs the service `invirtio@.service`.
- `enable` checks that the guest has a driver for the device, records it
  in `/etc/invirtio/devices.json`, adds a udev rule that keeps the host's
  driver from taking it at boot, and starts `invirtio@<name>`, now and at
  every boot. The name defaults to the type and the address, such as
  `wlan-06-00-0`. Enabling a device that is already recorded finishes
  enabling it, after an earlier `enable` that stopped partway.
- `disable` stops the guest, removes what a failed guest left on the host,
  gives the device back to its host driver, and removes the device's
  record and udev rule.
- `list` shows the host's WiFi and Ethernet devices, with the host driver
  that has each now, the guest driver that would drive it, and the name it
  is enabled as.
- `status` shows each device's service, its host and guest drivers, its
  VDUSE and vDPA state and its host interface. With `-v`, it adds the
  guest's VDUSE counters.
- `console` attaches to the guest's console, with the last 64 KiB of its
  output, and Ctrl-] detaches. Escape sequences that carry strings, C1
  controls and invalid UTF-8 are dropped from the guest's output, which
  assumes a terminal in UTF-8, as the console sets it when it attaches.

`devices.json` also takes, for each device, `memory` (the guest's `mem=`),
`memory_max` (the guest cgroup's limit in bytes), `options` for the
backend's `vduse_guest.device=` argument, `args` for the guest's command
line, and `irq_cpus` for the launcher's `-a`. A change takes effect when
the service restarts.

## The service

`invirtio@<name>` runs `invirtio run <name>`, which:

1. loads the host modules, and removes a wlan device that an earlier guest
   left on the vDPA bus, since a new guest cannot take one over;
2. gives the guest a cgroup of its own below the service's, with a memory
   limit;
3. runs the launcher on a pty, as the `invirtio` user, with the device and
   its in-flight log, which is on a small tmpfs of its own at
   `/run/invirtio/<name>`, since the guest can write to it without bound;
4. attaches the device with `vdpa dev add` once the guest has created it;
5. serves the console on `/run/invirtio/<name>.console`, which only root
   can open, and writes the guest's console to the journal, filtered as
   the console is. The lines marked `<name> launcher:` hold the launcher's
   messages and the guest's stderr, which the guest shares with it;
6. on stop, removes the device first, while the guest can answer the
   host's reset, then stops the guest.

When a guest exits by itself, a wlan device is removed, and a net device
stays on the bus for the next guest to reconnect to. systemd then starts
the service again. If the service is stopped before it restarts, or gives
up restarting, the net device stays on the bus with no guest, which
`status` shows, and `disable` removes it. The host's next message to such a
device waits for VDUSE's 30 s timeout, after which VDUSE treats the device
as broken until it is removed.
