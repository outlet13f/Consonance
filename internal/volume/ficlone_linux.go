//go:build linux && (amd64 || arm64)

package volume

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// ficloneRequest is the FICLONE ioctl, _IOW(0x94, 9, int), as encoded on amd64 and arm64.
const ficloneRequest = 0x40049409

// ficlone makes dst share all of src's extents.
func ficlone(dst, src *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficloneRequest, src.Fd())
	runtime.KeepAlive(dst)
	runtime.KeepAlive(src)
	switch errno {
	case 0:
		return nil
	case syscall.EOPNOTSUPP, syscall.EXDEV, syscall.ENOTTY, syscall.EINVAL:
		return fmt.Errorf("%w: %v", ErrReflinkUnsupported, errno)
	default:
		return errno
	}
}
