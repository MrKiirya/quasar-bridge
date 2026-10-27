package indicator

import (
	"errors"
	"os"
	"syscall"
)

const sysfsMagic = 0x62656572

type sysMounter struct{}

func (sysMounter) Bind(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

func (sysMounter) Unbind(target string) error { return syscall.Unmount(target, syscall.MNT_DETACH) }

func (sysMounter) Real(f *os.File) error {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Type != sysfsMagic {
		return errors.New("not a sysfs file")
	}
	return nil
}
