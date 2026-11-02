package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"invirtio/internal/command"
	"invirtio/internal/pci"
	"invirtio/internal/vdpa"
)

func cmdEnable(args []string) error {
	fs := flag.NewFlagSet("enable", flag.ExitOnError)
	name := fs.String("name", "", "the device's name (default <type>-<address>)")
	memory := fs.String("memory", "", "the guest's memory, such as 768M")
	rom := fs.String("rom", "", "for a GPU, the expansion ROM image that its guest sees, such as the VBIOS of an APU")
	mesa := fs.String("mesa", defaultMesaDir, "for a GPU, the Mesa prefix that graphical sessions take, which scripts/mesa-build.sh builds")
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio enable [-n] [-y] [-name <name>] [-memory <size>] [-rom <file>] [-mesa <prefix>] <pci address>\n\nRun the device's driver in a guest, now and at every boot, and export the\ndevice to the host as a virtio device. The host's own driver lets go of it.\nA GPU's guest shows the host's display manager on the GPU's connectors\nuntil the next boot instead, and the host's own driver drives it at boot.\nEach stage shows its commands and waits for Enter before it runs them.\nEnabling a device that is already recorded finishes enabling it.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	addr, err := pci.NormalizeAddress(fs.Arg(0))
	if err != nil {
		return err
	}
	typ, err := pci.Type(addr)
	if err != nil {
		return err
	}
	kernel, _, modinfo := guestFiles(typ)
	if _, err := os.Stat(kernel); err != nil {
		if typ == "gpu" {
			return errors.New("the guest of GPUs is not installed, run invirtio install with -gpu-guest")
		}
		return errors.New("InvirtIO is not installed, run invirtio install")
	}
	drv, err := pci.GuestDriver(addr, modinfo)
	if err != nil {
		return err
	}
	mesaSet := false
	fs.Visit(func(f *flag.Flag) { mesaSet = mesaSet || f.Name == "mesa" })
	if typ == "gpu" {
		if _, err := displayManager(); err != nil {
			return err
		}
		if !*dry && graphicalSession() {
			return errors.New("enabling a GPU ends the graphical sessions, so run it from a text console or a remote login")
		}
		if *mesa, err = filepath.Abs(*mesa); err != nil {
			return err
		}
		if *rom != "" {
			if *rom, err = filepath.Abs(*rom); err != nil {
				return err
			}
			if _, err := os.Stat(*rom); err != nil {
				return err
			}
		}
	} else if *rom != "" || mesaSet {
		return errors.New("-rom and -mesa are only for GPUs")
	}
	if err := pci.CheckGroup(addr); err != nil {
		return err
	}
	if *memory != "" {
		if _, err := parseSize(*memory); err != nil {
			return err
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// Only one process can open a VFIO group, so two devices in one group
	// cannot each have a guest.
	group, _ := pci.IOMMUGroup(addr)
	for n, d := range cfg.Devices {
		if g, err := pci.IOMMUGroup(d.Address); err == nil && g == group && d.Address != addr {
			return fmt.Errorf("%s is in IOMMU group %s with %s, which is enabled as %s", addr, group, d.Address, n)
		}
	}
	var record []action
	if n, d, err := cfg.lookup(addr); err == nil {
		if (*name != "" && *name != n) || (*memory != "" && *memory != d.Memory) ||
			(*rom != "" && !sameContent(*rom, d.ROM)) || (mesaSet && *mesa != d.Mesa) {
			return fmt.Errorf("%s is already recorded as %s, disable it to change it", addr, n)
		}
		*name = n
		*mesa = d.Mesa
	} else {
		if *name == "" {
			*name = defaultName(typ, addr)
		}
		if err := checkName(*name); err != nil {
			return err
		}
		if _, ok := cfg.Devices[*name]; ok {
			return fmt.Errorf("a device named %s is already enabled", *name)
		}
		dev := &Device{Address: addr, Type: typ, Memory: *memory}
		if typ == "gpu" {
			// BAR0 is the VRAM aperture of an AMD GPU, which the guest
			// writes, and the guest gives the host's display manager the
			// connectors without fbdev emulation of its own.
			dev.WCBars = "0x1"
			dev.Mesa = *mesa
			dev.Options = []string{"display"}
			dev.Args = []string{"drm_kms_helper.fbdev_emulation=0"}
			if *rom != "" {
				dev.ROM = romPath(*name)
				record = append(record, mkdirAction(configDir, 0755), copyAction(*rom, dev.ROM, 0644))
			}
		}
		cfg.Devices[*name] = dev
		a, err := configAction(cfg)
		if err != nil {
			return err
		}
		record = append(record, a)
	}

	host := pci.Driver(addr)
	if host == "" {
		host = "no driver"
	}
	fmt.Printf("%s is a %s device, which the guest drives with %s. On the host it has %s.\n\n",
		addr, typ, drv, host)
	unit := "invirtio@" + *name + ".service"
	if typ == "gpu" {
		if err := checkMesa(*mesa); err != nil {
			return err
		}
		stages := []stage{
			{title: "record the device as " + *name, actions: record},
			{
				title: "switch the display manager to " + *name + " until the next boot",
				notes: "This ends the graphical sessions. A transient unit stops the display\n" +
					"manager, makes it bind to " + unit + " and graphical sessions take\n" +
					*mesa + ", and starts the display manager, which starts the guest\n" +
					"first. If it does not start, everything goes back to the host's driver.\n" +
					"Its log: journalctl -u invirtio-display-" + *name,
				actions: []action{displayAction(true, *name)},
			},
		}
		if err := runStages(stages, *dry, *yes); err != nil {
			return err
		}
		if !*dry {
			fmt.Printf("\nThe display manager runs on %s until the next boot. Switch back with: invirtio disable %s\n", *name, *name)
		}
		return nil
	}
	// The udev rule comes last, so that a device whose guest never started
	// does not lose its host driver at the next boot.
	stages := []stage{
		{title: "record the device as " + *name, actions: record},
		{
			title: "start the guest now and at every boot",
			notes: "The service then moves the device to vfio-pci, starts the guest as the\n" +
				guestUser + " user, and attaches the guest's device with vdpa dev add.",
			actions: []action{cmdAction("systemctl", "enable", "--now", unit)},
		},
		{
			title:   "keep the host's driver from the device at boot",
			actions: udevActions(cfg),
		},
	}
	if err := runStages(stages, *dry, *yes); err != nil {
		return err
	}
	if !*dry {
		fmt.Printf("\nEnabled %s. Check it with: invirtio status %s\n", *name, *name)
	}
	return nil
}

func cmdDisable(args []string) error {
	fs := flag.NewFlagSet("disable", flag.ExitOnError)
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio disable [-n] [-y] <name | pci address>\n\nStop the device's guest and return the device to its host driver. Each\nstage shows its commands and waits for Enter before it runs them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	key := strings.ToLower(fs.Arg(0))
	if addr, err := pci.NormalizeAddress(key); err == nil {
		key = addr
	}
	name, dev, err := cfg.lookup(key)
	if err != nil {
		return err
	}
	if dev.Type == "gpu" && !*dry && graphicalSession() {
		return errors.New("disabling a GPU can end the graphical sessions, so run it from a text console or a remote login")
	}

	stages, err := disablePlan(cfg, name, dev)
	if err != nil {
		return err
	}
	if err := runStages(stages, *dry, *yes); err != nil {
		return err
	}
	if !*dry {
		drv := pci.Driver(dev.Address)
		if drv == "" {
			drv = "no driver"
		}
		fmt.Printf("\nDisabled %s. %s now has %s.\n", name, dev.Address, drv)
	}
	return nil
}

// disablePlan removes a device from cfg and returns the stages that disable
// it. The udev rule goes first, so that a device returned to its host driver
// keeps it at the next boot, wherever the disable stops.
func disablePlan(cfg *Config, name string, dev *Device) ([]stage, error) {
	delete(cfg.Devices, name)
	stages := []stage{{
		title:   "let the host's driver take the device at boot",
		actions: udevActions(cfg),
	}}
	stages = append(stages, disableStages(name, dev)...)
	record, err := configAction(cfg)
	if err != nil {
		return nil, err
	}
	forget := []action{record}
	if dev.ROM != "" {
		forget = append(forget, removeAction(dev.ROM))
	}
	return append(stages, stage{title: "forget the device", actions: forget}), nil
}

// disableStages stops a device's guest, removes what a failed guest left on
// the host, and returns the device to its host driver. A GPU's display
// manager goes back to the host's driver first.
func disableStages(name string, dev *Device) []stage {
	if dev.Type != "gpu" {
		return guestStages(name, dev)
	}
	return append([]stage{{
		title: name + ": switch the display manager back to the host's driver",
		notes: "This ends the graphical sessions if the display manager runs on the guest.\n" +
			"A transient unit stops it, stops the guest, returns the GPU to its host\n" +
			"driver and starts it again. Its log: journalctl -u invirtio-display-" + name,
		actions: []action{displayAction(false, name)},
	}}, guestStages(name, dev)...)
}

// guestStages stops a device's guest, removes what a failed guest left on
// the host, and returns the device to its host driver.
func guestStages(name string, dev *Device) []stage {
	unit := "invirtio@" + name + ".service"
	vduse := "/sys/class/vduse/" + name
	addr := dev.Address
	// A device that is gone from the PCI bus has no driver to return to.
	present := func() bool {
		_, err := os.Stat(pci.Path(addr, ""))
		return err == nil
	}
	onVFIO := func() bool { return pci.Driver(addr) == "vfio-pci" }
	unbound := func() bool { return present() && pci.Driver(addr) == "" }
	return []stage{
		{
			title: name + ": stop the guest and keep it from starting at boot",
			notes: "On stop, the service removes the device from the host before it stops\nthe guest.",
			actions: []action{{
				show: showCmd("systemctl", "disable", "--now", unit),
				run: func() error {
					err := command.Run("systemctl", "disable", "--now", unit)
					if err == nil {
						return nil
					}
					// After an uninstall that stopped partway, the unit
					// file can be gone, and the service can only be
					// stopped.
					if _, serr := os.Stat(unitPath); errors.Is(serr, os.ErrNotExist) {
						command.Run("systemctl", "stop", unit)
						return nil
					}
					return err
				},
			}},
		},
		{
			title: name + ": remove what a failed guest left on the host",
			notes: "A step is skipped when there is nothing for it to remove.",
			actions: []action{
				sysfsAction(vduse+"/msg_timeout", "1", func() bool { return vdpa.Attached(name) }),
				{
					show: showCmd("vdpa", "dev", "del", name),
					run: func() error {
						return vdpa.Remove(name, true)
					},
				},
				{
					show: "# the VDUSE_DESTROY_DEV ioctl for " + name + " on /dev/vduse/control, which no command makes",
					run:  func() error { return vdpa.Destroy(name) },
				},
				{
					show: showCmd("rm", "-f", logPath(name)),
					run: func() error {
						if err := os.Remove(logPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
							return err
						}
						return nil
					},
				},
				{
					show: showCmd("umount", logDir(name)),
					// The stopped guest can leave the log's fd in a cycle
					// of Unix sockets, which the kernel frees soon after,
					// so a busy tmpfs is tried for a second.
					run: func() error {
						for n := 0; isMountPoint(logDir(name)); n++ {
							err := unix.Unmount(logDir(name), 0)
							if err == nil || err != unix.EBUSY || n == 100 {
								return err
							}
							time.Sleep(10 * time.Millisecond)
						}
						return nil
					},
				},
				{
					show: showCmd("rmdir", logDir(name)),
					run: func() error {
						if err := os.Remove(logDir(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
							return err
						}
						return nil
					},
				},
			},
		},
		{
			title: name + ": return " + addr + " to its host driver",
			notes: "The unbind is skipped unless vfio-pci has the device, and the probe\nunless no driver has it. All three are skipped for a device that is gone\nfrom the PCI bus.",
			actions: []action{
				sysfsAction(pci.Path(addr, "driver_override"), "", present),
				sysfsAction(pci.Path(addr, "driver/unbind"), addr, onVFIO),
				sysfsAction("/sys/bus/pci/drivers_probe", addr, unbound),
			},
		},
	}
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio uninstall [-n] [-y]\n\nDisable every device, returning it to its host driver, and remove what\ninstall added. Each stage shows its commands and waits for Enter before it\nruns them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, d := range cfg.Devices {
		if d.Type == "gpu" && !*dry && graphicalSession() {
			return errors.New("uninstalling with a GPU enabled can end the graphical sessions, so run it from a text console or a remote login")
		}
	}

	stages := []stage{{
		title:   "let the host's drivers take the devices at boot",
		actions: udevActions(&Config{}),
	}}
	for _, name := range cfg.names() {
		stages = append(stages, disableStages(name, cfg.Devices[name])...)
	}
	// The user goes before the files, and this program last, so that a
	// failed step can be run again with the installed program.
	var remove []action
	if _, err := user.Lookup(guestUser); err == nil {
		remove = append(remove, cmdAction("userdel", guestUser))
	}
	remove = append(remove,
		cmdAction("rm", "-f", unitPath),
		cmdAction("systemctl", "daemon-reload"),
		cmdAction("udevadm", "control", "--reload"),
		cmdAction("rm", "-rf", libDir, configDir, runDir),
		cmdAction("rm", "-f", binPath))
	stages = append(stages, stage{title: "remove InvirtIO", actions: remove})
	if err := runStages(stages, *dry, *yes); err != nil {
		return err
	}
	if !*dry {
		fmt.Println("\nUninstalled InvirtIO.")
	}
	return nil
}

// configAction writes cfg as the configuration, through a temporary file and
// a rename.
func configAction(cfg *Config) (action, error) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return action{}, err
	}
	a := writeAction(configPath, string(data)+"\n", 0644)
	a.run = cfg.save
	return a, nil
}

// udevActions write the rules that keep each enabled device from its host
// driver at boot, before the driver's module loads for it. A device that the
// driver takes first is moved to vfio-pci when its guest starts.
func udevActions(cfg *Config) []action {
	var write action
	if len(cfg.Devices) == 0 {
		write = action{
			show: showCmd("rm", "-f", udevRulePath),
			run: func() error {
				if err := os.Remove(udevRulePath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return nil
			},
		}
	} else {
		var b strings.Builder
		b.WriteString("# Written by invirtio enable and disable.\n")
		for _, name := range cfg.names() {
			// A GPU keeps its host driver at boot.
			if cfg.Devices[name].Type == "gpu" {
				continue
			}
			fmt.Fprintf(&b, "ACTION==\"add\", SUBSYSTEM==\"pci\", KERNEL==\"%s\", ATTR{driver_override}=\"vfio-pci\"\n",
				cfg.Devices[name].Address)
		}
		write = writeAction(udevRulePath, b.String(), 0644)
	}
	return []action{write, cmdAction("udevadm", "control", "--reload")}
}

// removeAction removes a file, unless it is gone.
func removeAction(path string) action {
	return action{
		show: showCmd("rm", "-f", path),
		run: func() error {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}

// sameContent reports whether two files hold the same bytes.
func sameContent(a, b string) bool {
	x, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	y, err := os.ReadFile(b)
	return err == nil && bytes.Equal(x, y)
}
