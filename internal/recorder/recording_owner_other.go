//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package recorder

import (
	"errors"
	"os"
)

func validateRecordingDirectoryOwner(os.FileInfo) error {
	return errors.New("recording directory ownership validation is unsupported on this platform")
}
