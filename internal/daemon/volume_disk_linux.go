package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type linuxVolumeDisk struct {
	dir   string
	mkfs  string
	mount string
}

func openVolumeDisk(stateDir string) (volumeDisk, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("volumes require a privileged local Linux daemon")
	}
	base, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return nil, err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return nil, err
	}
	mount, err := exec.LookPath("mount")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, "volumes")
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("volume storage requires a private directory")
	}
	if err := syncDirectory(base); err != nil {
		return nil, err
	}
	return &linuxVolumeDisk{dir: dir, mkfs: mkfs, mount: mount}, nil
}

func (d *linuxVolumeDisk) path(id string) string  { return filepath.Join(d.dir, id, "data") }
func (d *linuxVolumeDisk) image(id string) string { return filepath.Join(d.dir, id, "disk.img") }

func volumeCommand(binary string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(binary), err, out)
	}
	return nil
}

func (d *linuxVolumeDisk) create(v Volume) error {
	dir := filepath.Join(d.dir, v.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(d.image(v.ID), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = f.Truncate(v.SizeBytes); err == nil {
		err = volumeCommand(d.mkfs, "-q", "-F", "-m", "0", "-E", "nodiscard,lazy_itable_init=0,lazy_journal_init=0", d.image(v.ID))
	}
	if err == nil {
		err = unix.Fallocate(int(f.Fd()), 0, 0, v.SizeBytes)
	}
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err := os.Mkdir(d.path(v.ID), 0o700); err != nil {
		return err
	}
	if err := d.ensure(v); err != nil {
		return err
	}
	if err := os.Chmod(d.path(v.ID), 0o777); err != nil {
		return err
	}
	if err := syncDirectory(d.path(v.ID)); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return syncDirectory(d.dir)
}

func (d *linuxVolumeDisk) ensure(v Volume) error {
	mounted, err := d.mounted(v)
	if mounted || err != nil {
		return err
	}
	if err := volumeCommand(d.mount, "-t", "ext4", "-o", "loop,rw,nosuid,nodev", d.image(v.ID), d.path(v.ID)); err != nil {
		return err
	}
	mounted, err = d.mounted(v)
	if err != nil {
		return err
	}
	if !mounted {
		return errors.New("no verified ext4 volume mount")
	}
	return nil
}

func (d *linuxVolumeDisk) mounted(v Volume) (bool, error) {
	var image, dir, data unix.Stat_t
	if err := unix.Lstat(d.image(v.ID), &image); err != nil {
		return false, err
	}
	if image.Size != v.SizeBytes || image.Mode&unix.S_IFMT != unix.S_IFREG || image.Mode&0o077 != 0 {
		return false, errors.New("volume image is not a private regular file of the recorded size")
	}
	if err := unix.Lstat(filepath.Join(d.dir, v.ID), &dir); err != nil {
		return false, err
	}
	if dir.Mode&unix.S_IFMT != unix.S_IFDIR || dir.Mode&0o077 != 0 {
		return false, errors.New("volume record directory is not private")
	}
	if err := unix.Lstat(d.path(v.ID), &data); err != nil {
		return false, err
	}
	if data.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false, errors.New("volume mount point is not a directory")
	}
	if data.Dev == dir.Dev {
		return false, nil
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(d.path(v.ID), &fs); err != nil {
		return false, err
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC || fs.Flags&unix.ST_RDONLY != 0 || unix.Major(data.Dev) != 7 {
		return false, errors.New("volume is not a writable loop-mounted ext4 filesystem")
	}
	dev, err := os.Open(fmt.Sprintf("/dev/loop%d", unix.Minor(data.Dev)))
	if err != nil {
		return false, err
	}
	defer func() { _ = dev.Close() }()
	status, err := unix.IoctlLoopGetStatus64(int(dev.Fd()))
	if err != nil {
		return false, err
	}
	if status.Device != image.Dev || status.Inode != image.Ino || status.Offset != 0 || status.Sizelimit != 0 {
		return false, errors.New("volume mount does not match its backing image")
	}
	return true, nil
}

func (d *linuxVolumeDisk) remove(v Volume) error {
	dir, p := filepath.Join(d.dir, v.ID), d.path(v.ID)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("cannot remove unexpected volume directory")
	}
	if _, err := os.Lstat(p); err == nil {
		mounted, checkErr := d.mounted(v)
		if checkErr != nil {
			return checkErr
		}
		if mounted {
			if err := unix.Unmount(p, 0); err != nil {
				return err
			}
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(d.image(v.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(dir); err != nil {
		return err
	}
	return syncDirectory(d.dir)
}
