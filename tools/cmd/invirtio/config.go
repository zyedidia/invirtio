package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"invirtio/internal/pci"
)

// Where install puts InvirtIO.
const (
	libDir       = "/usr/lib/invirtio"
	binPath      = "/usr/local/sbin/invirtio"
	configDir    = "/etc/invirtio"
	configPath   = configDir + "/devices.json"
	unitPath     = "/etc/systemd/system/invirtio@.service"
	udevRulePath = "/etc/udev/rules.d/70-invirtio.rules"
	guestUser    = "invirtio"

	// The in-flight logs must be on a tmpfs, and need not outlive a reboot,
	// since the VDUSE devices do not.
	runDir = "/run/invirtio"

	// Each device's log is on a tmpfs of its own, since the guest can write
	// to its log without bound, and a full /run would hold up the host's
	// services. The size is just above the largest log that the launcher
	// lets the guest lay out.
	logTmpfsOptions = "size=33m,nr_inodes=8,mode=0700"
)

var (
	guestPath    = filepath.Join(libDir, "linux")
	initrdPath   = filepath.Join(libDir, "initramfs.cpio")
	launcherPath = filepath.Join(libDir, "invirtio-launch")
	modinfoPath  = filepath.Join(libDir, "modules.builtin.modinfo")
)

// Device is one host device that a guest drives and exports.
type Device struct {
	Address string `json:"address"`
	// Type is the backend that exports the device: wlan or net.
	Type string `json:"type"`
	// Memory is the guest's memory, as UML's mem= takes it.
	Memory string `json:"memory,omitempty"`
	// MemoryMax bounds all the memory of the guest's cgroup, in bytes.
	MemoryMax int64 `json:"memory_max,omitempty"`
	// Options are added to the guest's vduse_guest.device= argument.
	Options []string `json:"options,omitempty"`
	// Args are added to the guest's command line.
	Args []string `json:"args,omitempty"`
	// IRQCPUs, if set, are the host CPUs that the device's interrupts go to.
	IRQCPUs string `json:"irq_cpus,omitempty"`
}

type Config struct {
	Devices map[string]*Device `json:"devices"`
}

var defaultMemory = map[string]string{
	"wlan": "768M",
	"net":  "256M",
}

func (d *Device) memory() string {
	if d.Memory != "" {
		return d.Memory
	}
	return defaultMemory[d.Type]
}

// memoryMax returns the cgroup limit, by default the guest's memory with
// room for UML's host-side allocations and the pages it pins for DMA.
func (d *Device) memoryMax() (int64, error) {
	if d.MemoryMax > 0 {
		return d.MemoryMax, nil
	}
	mem, err := parseSize(d.memory())
	if err != nil {
		return 0, err
	}
	return mem + 512<<20, nil
}

// parseSize parses a size as UML's mem= does, with an optional K, M or G.
func parseSize(s string) (int64, error) {
	shift := 0
	num := s
	if s != "" {
		switch strings.ToUpper(s[len(s)-1:]) {
		case "K":
			shift = 10
		case "M":
			shift = 20
		case "G":
			shift = 30
		}
		if shift != 0 {
			num = s[:len(s)-1]
		}
	}
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil || n <= 0 ||
		fmt.Sprint(n) != num || n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("cannot parse the size %q", s)
	}
	return n << shift, nil
}

// A name is a VDUSE device name and a systemd instance name, and it appears
// in paths and on the guest's command line.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func checkName(name string) error {
	// The VDUSE control device is called control, and the console lines of
	// a device called invirtio would look like this program's messages.
	if !nameRE.MatchString(name) || name == "control" || name == "invirtio" {
		return fmt.Errorf("%q is not a valid device name, which has up to 32 lowercase letters, digits and dashes", name)
	}
	return nil
}

// defaultName names a device after its type and address, so that two ports
// of one card get different names.
func defaultName(typ, addr string) string {
	a := strings.TrimPrefix(addr, "0000:")
	a = strings.NewReplacer(":", "-", ".", "-").Replace(a)
	return typ + "-" + a
}

func loadConfig() (*Config, error) {
	cfg := &Config{Devices: map[string]*Device{}}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("%s: %v", configPath, err)
	}
	if cfg.Devices == nil {
		cfg.Devices = map[string]*Device{}
	}
	for name, d := range cfg.Devices {
		if err := checkName(name); err != nil {
			return nil, fmt.Errorf("%s: %v", configPath, err)
		}
		if _, ok := defaultMemory[d.Type]; !ok {
			return nil, fmt.Errorf("%s: device %s has the unknown type %q", configPath, name, d.Type)
		}
		if !pci.ValidAddress(d.Address) {
			return nil, fmt.Errorf("%s: device %s has the invalid address %q", configPath, name, d.Address)
		}
	}
	return cfg, nil
}

// save writes the configuration through a temporary file, so that a crash
// leaves either the old file or the new one.
func (c *Config) save() error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}

func (c *Config) names() []string {
	names := make([]string, 0, len(c.Devices))
	for name := range c.Devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookup finds a device by its name or its PCI address.
func (c *Config) lookup(key string) (string, *Device, error) {
	if d, ok := c.Devices[key]; ok {
		return key, d, nil
	}
	for name, d := range c.Devices {
		if d.Address == key {
			return name, d, nil
		}
	}
	return "", nil, fmt.Errorf("no device %s is enabled", key)
}

func (c *Config) device(name string) (*Device, error) {
	d, ok := c.Devices[name]
	if !ok {
		return nil, fmt.Errorf("no device %s is enabled", name)
	}
	return d, nil
}

func logDir(name string) string       { return filepath.Join(runDir, name) }
func logPath(name string) string      { return filepath.Join(logDir(name), "inflight") }
func mconsolePath(name string) string { return filepath.Join(runDir, name+".mconsole") }
func consolePath(name string) string  { return filepath.Join(runDir, name+".console") }
