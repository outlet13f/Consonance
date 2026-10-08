//go:build !(linux && (amd64 || arm64))

package volume

import (
	"fmt"
	"os"
	"runtime"
)

// ficlone is implemented only on Linux for amd64 and arm64.
func ficlone(dst, src *os.File) error {
	return fmt.Errorf("%w on %s/%s", ErrReflinkUnsupported, runtime.GOOS, runtime.GOARCH)
}
