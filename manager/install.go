package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
)

// The service runs one device's guest. KillMode=mixed sends the stop signal
// to the supervisor alone, which removes the device and stops the guest in
// order. Delegate lets the supervisor give the guest a cgroup of its own,
// and DelegateSubgroup starts the supervisor in a subgroup beside it. A stop
// can wait for VDUSE's message timeout and then for the launcher's, so
// TimeoutStopSec leaves room for both.
const unitText = `[Unit]
Description=InvirtIO driver guest for %i
After=systemd-udevd.service systemd-modules-load.service

[Service]
ExecStart=` + binPath + ` run %i
Restart=on-failure
RestartSec=2
KillMode=mixed
TimeoutStopSec=120
Delegate=yes
DelegateSubgroup=supervisor

[Install]
WantedBy=multi-user.target
`

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	guest := fs.String("guest", "", "the build directory of the guest, with linux and modules.builtin.modinfo")
	initrd := fs.String("initramfs", "", "the guest's initramfs (default <guest>/initramfs/initramfs.cpio)")
	launcher := fs.String("launcher", "", "the launcher (default invirtio-launch next to this program)")
	dry := fs.Bool("n", false, "show the commands of each stage without running them")
	yes := fs.Bool("y", false, "run each stage without waiting for Enter")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: invirtio install [-n] [-y] -guest <build dir> [-initramfs <file>] [-launcher <file>]\n\nInstall the guest, the launcher, this program and the service. Each stage\nshows its commands and waits for Enter before it runs them. Running guests\nkeep the old guest until they restart.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *guest == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	if !*dry {
		requireRoot()
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if *initrd == "" {
		*initrd = filepath.Join(*guest, "initramfs", "initramfs.cpio")
	}
	if *launcher == "" {
		*launcher = filepath.Join(filepath.Dir(self), "invirtio-launch")
	}
	files := []struct {
		src, dst string
		mode     os.FileMode
	}{
		{filepath.Join(*guest, "linux"), guestPath, 0755},
		{filepath.Join(*guest, "modules.builtin.modinfo"), modinfoPath, 0644},
		{*initrd, initrdPath, 0644},
		{*launcher, launcherPath, 0755},
		{self, binPath, 0755},
	}
	copies := []action{mkdirAction(libDir, 0755)}
	for _, f := range files {
		src, err := filepath.Abs(f.src)
		if err != nil {
			return err
		}
		if _, err := os.Stat(src); err != nil {
			return err
		}
		if same, _ := sameFile(src, f.dst); !same {
			copies = append(copies, copyAction(src, f.dst, f.mode))
		}
	}

	var users []action
	if _, err := user.Lookup(guestUser); err != nil {
		users = append(users, cmdAction("useradd", "--system", "--no-create-home",
			"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin",
			"--user-group", guestUser))
	}

	var configs []action
	if _, err := os.Stat(configPath); err != nil {
		configs = append(configs, mkdirAction(configDir, 0755),
			writeAction(configPath, emptyConfig, 0644))
	}

	stages := []stage{
		{title: "check that the host can run guests", actions: hostChecks()},
		{title: "copy the guest, the launcher and invirtio", actions: copies},
		{title: "add the user " + guestUser + ", which the guests run as", actions: users},
		{title: "install the service", actions: []action{
			writeAction(unitPath, unitText, 0644),
			cmdAction("systemctl", "daemon-reload"),
		}},
		{title: "create the device list", actions: configs},
	}
	if err := runStages(stages, *dry, *yes); err != nil {
		return err
	}
	if *dry {
		return nil
	}

	fmt.Println("\nInstalled InvirtIO. Enable a device with: invirtio enable <pci address>")
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, name := range cfg.names() {
		if systemctlState("is-active", "invirtio@"+name+".service") == "active" {
			fmt.Printf("The guest of %s runs the old version until: systemctl restart invirtio@%s\n", name, name)
		}
	}
	return nil
}

const emptyConfig = `{
  "devices": {}
}
`

// hostChecks checks that the host kernel can run guests. A missing
// virtio_wlan or cgroup v2 hierarchy limits what guests can do, and only
// gives a warning.
func hostChecks() []action {
	return []action{
		{
			show: "ls /sys/kernel/iommu_groups",
			run: func() error {
				groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
				if len(groups) == 0 {
					return errors.New("the host has no IOMMU groups, enable the IOMMU on the kernel command line")
				}
				return nil
			},
		},
		{
			show: showCmd("modprobe", "-n", "-a", "vfio-pci", "vduse", "vdpa", "virtio_vdpa"),
			run: func() error {
				if err := runCmd("modprobe", "-n", "-a", "vfio-pci", "vduse", "vdpa", "virtio_vdpa"); err != nil {
					return fmt.Errorf("the host kernel lacks a module: %v", err)
				}
				return nil
			},
		},
		{
			show: showCmd("modprobe", "-n", "virtio_wlan"),
			run: func() error {
				if err := runCmd("modprobe", "-n", "virtio_wlan"); err != nil {
					warn("the host kernel has no virtio_wlan, so WiFi devices cannot be exported")
				}
				return nil
			},
		},
		{
			show: "command -v vdpa",
			run: func() error {
				if _, err := exec.LookPath("vdpa"); err != nil {
					return errors.New("the vdpa command is missing, install iproute2")
				}
				return nil
			},
		},
		{
			show: "test -e /sys/fs/cgroup/cgroup.controllers",
			run: func() error {
				if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
					warn("the host has no cgroup v2 hierarchy, so the guests' memory is not bounded")
				}
				return nil
			},
		},
	}
}

// copyFile copies through a temporary file and a rename, so that a guest
// that runs the old file keeps it.
func copyFile(src, dst string, mode os.FileMode) error {
	if same, _ := sameFile(src, dst); same {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}
