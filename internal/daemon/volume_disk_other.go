//go:build !linux

package daemon

import "errors"

func openVolumeDisk(_ string) (volumeDisk, error) {
	return nil, errors.New("persistent volumes require a local Linux daemon")
}
