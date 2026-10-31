// Package sysfs reads and writes the small files of sysfs, procfs and the
// cgroup hierarchy.
package sysfs

import (
	"os"
	"strings"
)

// Read returns a file's contents without surrounding whitespace.
func Read(p string) (string, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Write writes to an existing file. Opening a missing one fails with
// ENOENT, where O_CREAT would give EACCES on kernfs.
func Write(p, value string) error {
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
