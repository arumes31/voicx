//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package recorder

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func validateRecordingDirectoryOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return errors.New("cannot determine existing recording directory owner")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf(
			"existing recording directory owner %d does not match process owner %d",
			stat.Uid,
			os.Geteuid(),
		)
	}
	return nil
}
