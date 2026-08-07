//go:build windows

package recorder

import "os"

func validateRecordingDirectoryOwner(os.FileInfo) error {
	// Config validation requires the operator's explicit acknowledgement that
	// this directory has a restricted inheritable NTFS DACL. Windows ownership
	// alone does not prove that access policy, so it is not used as a substitute.
	return nil
}
