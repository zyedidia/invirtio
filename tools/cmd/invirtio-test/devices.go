package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"invirtio/internal/harness"
	"invirtio/internal/mconsole"
	"invirtio/internal/pci"
	"invirtio/internal/sysfs"
	"invirtio/internal/vdpa"
)

func init() {
	register("bind", "detach a PCI device from its host driver and bind it to vfio-pci", cmdBind)
	register("unbind", "return a PCI device from vfio-pci to its host driver", cmdUnbind)
	register("platform-bind", "bind a platform device to vfio-platform", cmdPlatformBind)
	register("platform-unbind", "return a platform device from vfio-platform to its host driver", cmdPlatformUnbind)
	register("irq-affinity", "steer the host interrupts of a VFIO device to host CPUs", cmdIRQAffinity)
	register("destroy", "destroy a VDUSE device that a killed guest left behind", cmdDestroy)
	register("run", "run a guest that serves a VDUSE device and attach the device", cmdRun)
	register("mconsole", "send a command to a UML guest's management console", cmdMconsole)
}

// newFlags returns a flag set whose usage prints the text before the flags.
func newFlags(name, text string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(text))
		fs.PrintDefaults()
	}
	return fs
}

func pciArg(fs *flag.FlagSet) string {
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	addr, err := pci.NormalizeAddress(fs.Arg(0))
	if err != nil {
		fatal("%v", err)
	}
	if !harness.Exists(pci.Path(addr, "")) {
		fatal("no such PCI device: %s", addr)
	}
	return addr
}

func cmdBind(args []string) error {
	fs := newFlags("bind", `
usage: invirtio-test bind <pci address>

Bind a PCI device to vfio-pci, so that a guest can claim it. Refuses a
device that has a mounted block device or active swap behind it, so that
the disk the host runs from cannot be taken away.`)
	fs.Parse(args)
	addr := pciArg(fs)
	if uses := pci.InUse(addr); len(uses) > 0 {
		return fmt.Errorf("refusing to bind %s, it is in use:\n  %s\nunmount and swapoff those first",
			addr, strings.Join(uses, "\n  "))
	}
	requireRoot()

	group, err := pci.IOMMUGroup(addr)
	if err != nil {
		return err
	}
	fmt.Printf("%s is in IOMMU group %s\n", addr, group)
	if err := pci.CheckGroup(addr); err != nil {
		fmt.Fprintf(os.Stderr, "invirtio-test: %v, so bind those too\n", err)
	}
	if drv := pci.Driver(addr); drv == "vfio-pci" {
		fmt.Printf("%s is already bound to vfio-pci\n", addr)
		return nil
	} else if drv != "" {
		fmt.Printf("unbinding %s from %s\n", addr, drv)
	}
	if err := pci.BindVFIO(addr); err != nil {
		return err
	}
	fmt.Printf("%s bound to vfio-pci, /dev/vfio/%s\n", addr, group)
	return nil
}

// groupHolders returns the processes that hold /dev/vfio/<group> open,
// which keep the device from leaving vfio.
func groupHolders(group string) []string {
	node := "/dev/vfio/" + group
	seen := map[string]bool{}
	var pids []string
	fds, _ := filepath.Glob("/proc/[0-9]*/fd/*")
	for _, fd := range fds {
		if t, err := os.Readlink(fd); err == nil && t == node {
			pid := strings.Split(fd, "/")[2]
			if !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func cmdUnbind(args []string) error {
	fs := newFlags("unbind", `
usage: invirtio-test unbind <pci address>

Clear the vfio-pci override of a PCI device and let the kernel bind the
driver that matches it. Stop whatever uses the device first, since a
process that holds /dev/vfio/<group> open keeps it.`)
	fs.Parse(args)
	addr := pciArg(fs)
	requireRoot()

	cur := pci.Driver(addr)
	if cur != "" && cur != "vfio-pci" {
		fmt.Printf("%s is bound to %s, not vfio-pci, so there is nothing to do\n", addr, cur)
		return nil
	}
	if group, err := pci.IOMMUGroup(addr); err == nil {
		if pids := groupHolders(group); len(pids) > 0 {
			return fmt.Errorf("/dev/vfio/%s is still open by pid %s, stop them first",
				group, strings.Join(pids, ", "))
		}
	}
	if cur != "" {
		fmt.Printf("unbinding %s from %s\n", addr, cur)
	}
	if err := pci.BindNative(addr); err != nil {
		return err
	}
	drv := pci.Driver(addr)
	if drv == "" {
		return fmt.Errorf("%s is now unbound, no host driver matched it", addr)
	}
	fmt.Printf("%s bound to %s\n", addr, drv)
	blocks, _ := filepath.Glob("/sys/block/*")
	for _, b := range blocks {
		if dev, err := filepath.EvalSymlinks(filepath.Join(b, "device")); err == nil &&
			strings.Contains(dev+"/", "/"+addr+"/") {
			fmt.Printf("  block device /dev/%s\n", filepath.Base(b))
		}
	}
	return nil
}

func platformDriver(dev string) string {
	link, err := os.Readlink(filepath.Join(dev, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func platformArg(fs *flag.FlagSet) (string, string) {
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	name := fs.Arg(0)
	dev := filepath.Join("/sys/bus/platform/devices", name)
	if !harness.Exists(dev) {
		fatal("no such platform device: %s", name)
	}
	return name, dev
}

// usbAttached returns the USB devices behind a host controller, which hang
// off its root hubs. Devices are the directories with a devnum, which leaves
// out interfaces and hub ports.
func usbAttached(dev string) []string {
	var found []string
	hubs, _ := filepath.Glob(filepath.Join(dev, "usb[0-9]*"))
	for _, hub := range hubs {
		bus := strings.TrimPrefix(filepath.Base(hub), "usb")
		filepath.WalkDir(hub, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if strings.Count(strings.TrimPrefix(p, hub), "/") > 4 {
				return filepath.SkipDir
			}
			n := filepath.Base(p)
			if strings.HasPrefix(n, bus+"-") && harness.Exists(filepath.Join(p, "devnum")) {
				prod := harness.Read(filepath.Join(p, "product"))
				if prod == "" {
					prod = "?"
				}
				found = append(found, fmt.Sprintf("USB device %s (%s) is attached", n, prod))
			}
			return nil
		})
	}
	return found
}

func cmdPlatformBind(args []string) error {
	fs := newFlags("platform-bind", `
usage: invirtio-test platform-bind [-no-reset] <platform device>

Bind a platform device, such as an ACPI xHCI controller like NVDA8000:02, to
vfio-platform, so that a guest can claim it. The names are in
/sys/bus/platform/devices. vfio-platform refuses a device that it cannot
reset, which for ACPI devices means one without an _RST method. -no-reset
loads vfio-platform with reset_required=0, so that such a device can be
bound, and reloads it if it is loaded with the other setting and has no
devices. Refuses a USB host controller with devices attached.`)
	noReset := fs.Bool("no-reset", false, "load vfio-platform with reset_required=0")
	fs.Parse(args)
	name, dev := platformArg(fs)
	if uses := usbAttached(dev); len(uses) > 0 {
		return fmt.Errorf("refusing to bind %s, it is in use:\n  %s\nunplug those first",
			name, strings.Join(uses, "\n  "))
	}
	requireRoot()

	link, err := os.Readlink(filepath.Join(dev, "iommu_group"))
	if err != nil {
		return fmt.Errorf("%s has no IOMMU group, is an IOMMU enabled?", name)
	}
	group := filepath.Base(link)
	fmt.Printf("%s is in IOMMU group %s\n", name, group)
	peers, _ := filepath.Glob(filepath.Join("/sys/kernel/iommu_groups", group, "devices/*"))
	for _, p := range peers {
		if filepath.Base(p) != name {
			fmt.Fprintf(os.Stderr, "  group peer %s (driver %s) needs to be bound too\n",
				filepath.Base(p), platformDriver(p))
		}
	}

	cur := platformDriver(dev)
	if cur == "vfio-platform" {
		fmt.Printf("%s is already bound to vfio-platform\n", name)
		return nil
	}
	want := "Y"
	if *noReset {
		want = "N"
	}
	// reset_required is read-only once the module is loaded, so a mismatch
	// means reloading it.
	param := "/sys/module/vfio_platform/parameters/reset_required"
	if have := harness.Read(param); have != "" && have != want {
		if bound, _ := filepath.Glob("/sys/bus/platform/drivers/vfio-platform/*:*"); len(bound) > 0 {
			return fmt.Errorf("vfio-platform is loaded with reset_required=%s and has devices bound, unbind them first", have)
		}
		fmt.Printf("reloading vfio-platform with reset_required=%s\n", want)
		if err := harness.MustRun("modprobe", "-r", "vfio-platform"); err != nil {
			return err
		}
	}
	// A kernel built with IOMMUFD_VFIO_CONTAINER has no type1 module.
	harness.Run("modprobe", "-q", "vfio_iommu_type1")
	reset := "1"
	if *noReset {
		reset = "0"
	}
	if err := harness.MustRun("modprobe", "vfio-platform", "reset_required="+reset); err != nil {
		return err
	}

	// driver_override comes first, so that the probe cannot give the device
	// back to its own driver after the unbind.
	if err := sysfs.Write(filepath.Join(dev, "driver_override"), "vfio-platform"); err != nil {
		return err
	}
	if cur != "" {
		fmt.Printf("unbinding %s from %s\n", name, cur)
		if err := sysfs.Write(filepath.Join(dev, "driver/unbind"), name); err != nil {
			return err
		}
	}
	sysfs.Write("/sys/bus/platform/drivers_probe", name)
	if now := platformDriver(dev); now != "vfio-platform" {
		msg := fmt.Sprintf("binding %s failed, its driver is %q", name, now)
		var vfio []string
		for _, l := range harness.KernelLog() {
			if strings.Contains(strings.ToLower(l), "vfio") {
				vfio = append(vfio, l)
			}
		}
		for _, l := range vfio[max(0, len(vfio)-3):] {
			msg += "\n  " + l
		}
		if !*noReset {
			msg += "\nif the device has no reset function, retry with -no-reset"
		}
		return errors.New(msg + "\ninvirtio-test platform-unbind " + name + " returns it to the host")
	}
	fmt.Printf("%s bound to vfio-platform, /dev/vfio/%s\n", name, group)
	return nil
}

func cmdPlatformUnbind(args []string) error {
	fs := newFlags("platform-unbind", `
usage: invirtio-test platform-unbind <platform device>

Clear the vfio-platform override of a platform device and let the kernel
bind the driver that matches it. Stop whatever uses the device first.`)
	fs.Parse(args)
	name, dev := platformArg(fs)
	requireRoot()

	cur := platformDriver(dev)
	if cur != "" && cur != "vfio-platform" {
		fmt.Printf("%s is bound to %s, not vfio-platform, so there is nothing to do\n", name, cur)
		return nil
	}
	if link, err := os.Readlink(filepath.Join(dev, "iommu_group")); err == nil {
		group := filepath.Base(link)
		if pids := groupHolders(group); len(pids) > 0 {
			return fmt.Errorf("/dev/vfio/%s is still open by pid %s, stop them first",
				group, strings.Join(pids, ", "))
		}
	}
	// Clearing the override comes first, or the probe binds vfio-platform
	// again.
	if err := sysfs.Write(filepath.Join(dev, "driver_override"), "\n"); err != nil {
		return err
	}
	if cur != "" {
		fmt.Printf("unbinding %s from %s\n", name, cur)
		if err := sysfs.Write(filepath.Join(dev, "driver/unbind"), name); err != nil {
			return err
		}
	}
	sysfs.Write("/sys/bus/platform/drivers_probe", name)
	drv := platformDriver(dev)
	if drv == "" {
		return fmt.Errorf("%s is now unbound, no host driver matched it", name)
	}
	fmt.Printf("%s bound to %s\n", name, drv)
	return nil
}

var (
	cpuListRE = regexp.MustCompile(`^[0-9]+(-[0-9]+)?(,[0-9]+(-[0-9]+)?)*$`)
	vfioIRQRE = regexp.MustCompile(`^vfio-msix?\[`)
)

// vfioIRQs returns the host IRQs that vfio-pci requested for a device,
// named vfio-msi[N](<address>) or vfio-msix[N](<address>).
func vfioIRQs(addr string) []string {
	data, _ := os.ReadFile("/proc/interrupts")
	var irqs []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		last := f[len(f)-1]
		if vfioIRQRE.MatchString(last) && strings.Contains(last, "("+addr+")") {
			irqs = append(irqs, strings.TrimSuffix(f[0], ":"))
		}
	}
	return irqs
}

func cmdIRQAffinity(args []string) error {
	fs := newFlags("irq-affinity", `
usage: invirtio-test irq-affinity [-once] <pci address> <cpu list>

Set the affinity of the host interrupts that vfio-pci requested for the
device to the given host CPUs, such as 4 or 4-5. The interrupts exist only
while the guest has MSI or MSI-X enabled, and are requested again when the
guest switches modes or reloads its driver, so by default the command keeps
running and sets the affinity of each interrupt when it appears, until it is
interrupted. With -once, it sets the affinity of the interrupts that exist
now and fails if there are none. Pin the guest CPU that handles the
interrupts (cpu_pin=) to the same host CPU.`)
	once := fs.Bool("once", false, "set the affinity once and exit")
	fs.Parse(args)
	if fs.NArg() != 2 {
		fs.Usage()
		os.Exit(2)
	}
	addr, err := pci.NormalizeAddress(fs.Arg(0))
	if err != nil {
		return err
	}
	cpus := fs.Arg(1)
	if !cpuListRE.MatchString(cpus) {
		return fmt.Errorf("%q is not a CPU list like 4 or 4-5", cpus)
	}
	requireRoot()

	// The kernel prints the list in its own form, such as 4-5 for 4,5,
	// which is known after the first write.
	target := cpus
	apply := func() int {
		irqs := vfioIRQs(addr)
		for _, irq := range irqs {
			p := "/proc/irq/" + irq + "/smp_affinity_list"
			// An interrupt can be freed at any time, so errors skip it.
			cur, err := sysfs.Read(p)
			if err != nil || cur == target {
				continue
			}
			if sysfs.Write(p, cpus) != nil {
				fmt.Fprintf(os.Stderr, "failed to set the affinity of IRQ %s\n", irq)
				continue
			}
			if cur, err := sysfs.Read(p); err == nil {
				target = cur
				fmt.Fprintf(os.Stderr, "IRQ %s -> CPU %s\n", irq, target)
			}
		}
		return len(irqs)
	}
	if *once {
		if apply() == 0 {
			return fmt.Errorf("%s has no VFIO interrupts", addr)
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "watching the VFIO interrupts of %s, stop with Ctrl-C\n", addr)
	for {
		apply()
		time.Sleep(200 * time.Millisecond)
	}
}

func cmdDestroy(args []string) error {
	fs := newFlags("destroy", `
usage: invirtio-test destroy <name>

Destroy a VDUSE device that a guest left behind when it was killed rather
than stopped. A device on the vDPA bus must be removed first with vdpa dev
del, and its guest must have exited.`)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()
	name := fs.Arg(0)
	if !vdpa.Exists(name) {
		return fmt.Errorf("no VDUSE device %s", name)
	}
	if vdpa.Attached(name) {
		return fmt.Errorf("%s is on the vDPA bus, run vdpa dev del %s first", name, name)
	}
	return vdpa.Destroy(name)
}

func cmdRun(args []string) error {
	fs := newFlags("run", `
usage: invirtio-test run [-log <file>] <name> <guest command>...

Run a UML guest that serves the VDUSE device <name>, attach the device to the
host once the guest has created it, and remove and destroy the device when
the guest exits, however it exits. The guest's console stays on the terminal
and is also written to the log. A device that an earlier guest left behind
is removed first, since a wlan device cannot be taken over. A signal stops
the guest in order.`)
	logPath := fs.String("log", "", "the console log (default /tmp/vduse-run-<name>.log)")
	fs.Parse(args)
	if fs.NArg() < 2 {
		fs.Usage()
		os.Exit(2)
	}
	requireRoot()
	name := fs.Arg(0)
	if *logPath == "" {
		*logPath = "/tmp/vduse-run-" + name + ".log"
	}
	if err := harness.Modprobe("vduse", "vdpa", "virtio_vdpa"); err != nil {
		return err
	}
	harness.Cleanup(name)
	logf, err := os.Create(*logPath)
	if err != nil {
		return err
	}

	// A guest that is killed leaves the terminal raw, so it is restored when
	// the guest exits.
	in := int(os.Stdin.Fd())
	restore := func() {}
	if term.IsTerminal(in) {
		if state, err := term.GetState(in); err == nil {
			restore = func() { term.Restore(in, state) }
		}
	}

	// The guest's output goes to the terminal and the log, and signals reach
	// the guest as SIGTERM, which powers it off in order, so that its
	// shutdown is logged and the device is always removed.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	guest := exec.Command(fs.Arg(1), fs.Args()[2:]...)
	guest.Stdin = os.Stdin
	out, err := guest.StdoutPipe()
	if err != nil {
		return err
	}
	guest.Stderr = guest.Stdout
	if err := guest.Start(); err != nil {
		return err
	}
	var logMu sync.Mutex
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		buf := make([]byte, 4096)
		for {
			n, err := out.Read(buf)
			if n > 0 {
				os.Stdout.Write(buf[:n])
				logMu.Lock()
				logf.Write(buf[:n])
				logMu.Unlock()
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "invirtio-test: %v\n", err)
				}
				return
			}
		}
	}()

	// The guest names the device in its console once it has created it.
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			if strings.Contains(harness.Read(*logPath), name+": created") {
				if err := vdpa.Add(name); err != nil {
					fmt.Fprintf(os.Stderr, "invirtio-test: %v\n", err)
				}
				return
			}
		}
	}()

	exited := make(chan error, 1)
	go func() {
		<-copied
		exited <- guest.Wait()
	}()
	var waitErr error
wait:
	for {
		select {
		case <-sigs:
			guest.Process.Signal(syscall.SIGTERM)
		case waitErr = <-exited:
			break wait
		}
	}

	// A lowered timeout keeps an add that the watcher started from waiting
	// long for a guest that is gone.
	close(stop)
	sysfs.Write(filepath.Join("/sys/class/vduse", name, "msg_timeout"), "1")
	<-watched
	harness.Cleanup(name)
	restore()
	logf.Close()

	// The exit status is the guest's, as a shell gives it for a guest that
	// a signal killed.
	var exit *exec.ExitError
	if errors.As(waitErr, &exit) {
		code := exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		os.Exit(code)
	}
	return waitErr
}

func cmdMconsole(args []string) error {
	fs := newFlags("mconsole", `
usage: invirtio-test mconsole <umid | socket path> <command>...

Send one command to a running UML guest's management console and print the
reply. The umid is the one given to the guest with umid=, and its socket is
~/.uml/<umid>/mconsole of the user who runs the guest. A guest run by the
launcher with -s serves the socket at that path. Commands include
"proc interrupts" and "help".`)
	fs.Parse(args)
	if fs.NArg() < 2 {
		fs.Usage()
		os.Exit(2)
	}
	path := fs.Arg(0)
	if !strings.Contains(path, "/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		path = filepath.Join(home, ".uml", path, "mconsole")
	}
	text, err := mconsole.Request(path, strings.Join(fs.Args()[1:], " "))
	if err != nil {
		return err
	}
	text = strings.TrimLeft(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	os.Stdout.WriteString(text)
	return nil
}
