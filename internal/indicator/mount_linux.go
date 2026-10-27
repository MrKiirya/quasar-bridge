package indicator

import "syscall"

type sysMounter struct{}

func (sysMounter) Bind(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

func (sysMounter) Unbind(target string) error { return syscall.Unmount(target, 0) }
