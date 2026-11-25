package main

import (
	"math"
	"testing"
)

func TestVolumeMountCoversWholeSubtree(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		suffix string
		valid  bool
	}{{"/sub", false}, {"-sibling", true}, {"", false}} {
		info := "5 1 7:1 / " + root + " ro - ext4 /dev/loop1 rw\n6 1 0:8 / " + root + tc.suffix + " rw - tmpfs tmpfs rw\n"
		if err := verifyVolumeMount(root, math.MaxUint64, true, info); (err == nil) != tc.valid {
			t.Errorf("suffix %q: %v", tc.suffix, err)
		}
	}
}
