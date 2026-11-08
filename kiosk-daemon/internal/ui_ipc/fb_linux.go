//go:build linux

package ui_ipc

import (
	"syscall"
	"unsafe"
)

func mmapFB(fd uintptr, length int) ([]byte, error) {
	return syscall.Mmap(int(fd), 0, length, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}

func syncVBlank(fd uintptr) {
	var crtc uint32 = 0
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, 0x40044620, uintptr(unsafe.Pointer(&crtc)))
}
