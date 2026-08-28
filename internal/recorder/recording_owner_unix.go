//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package recorder

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"voicx/internal/safecast"
)

func validateRecordingDirectoryOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return errors.New("cannot determine existing recording directory owner")
	}
	euid := os.Geteuid()
	expectedUID, err := safecast.IntToUint32(euid)
	if err != nil {
		return fmt.Errorf("converting effective user ID: %w", err)
	}
	if stat.Uid != expectedUID {
		return fmt.Errorf(
			"existing recording directory owner %d does not match process owner %d",
			stat.Uid,
			euid,
		)
	}
	return nil
}
