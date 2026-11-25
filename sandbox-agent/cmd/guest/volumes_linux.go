package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func verifyVolumeMounts(args []string) error {
	if len(args) == 0 || len(args)%3 != 0 {
		return errors.New("invalid volume mount arguments")
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for i := 0; i < len(args); i += 3 {
		p := args[i]
		n, err := strconv.ParseUint(args[i+1], 10, 64)
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil || resolved != p {
			return fmt.Errorf("volume target is not a canonical guest directory: %s", p)
		}
		ro := args[i+2] == "true"
		if err := verifyVolumeMount(p, n, ro, string(data)); err != nil {
			return fmt.Errorf("volume at %s: %w", p, err)
		}
	}
	return nil
}

func verifyVolumeMount(p string, size uint64, readOnly bool, mountinfo string) error {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	found := false
	for line := range strings.Lines(mountinfo) {
		f := strings.Fields(line)
		if len(f) > 5 && strings.HasPrefix(f[4], p+"/") {
			return errors.New("another filesystem is mounted below the volume")
		}
		if len(f) > 5 && f[4] == p {
			if found || !slices.Contains(strings.Split(f[5], ","), mode) {
				return errors.New("ambiguous volume mount or wrong access mode")
			}
			found = true
		}
	}
	if !found {
		return errors.New("volume is not a distinct guest mount")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(p, &fs); err != nil {
		return err
	}
	if fs.Bsize <= 0 || fs.Blocks == 0 || fs.Blocks > size/uint64(fs.Bsize) {
		return errors.New("volume does not have the configured storage bound")
	}
	return nil
}
