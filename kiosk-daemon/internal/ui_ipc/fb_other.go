//go:build !linux

package ui_ipc

import (
	"errors"
)

func mmapFB(fd uintptr, length int) ([]byte, error) {
	return nil, errors.New("mmap not supported on non-linux platforms")
}

func syncVBlank(fd uintptr) {
}
