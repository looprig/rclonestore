//go:build windows

package rclonestore

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

const windowsErrorNotSupported syscall.Errno = 50

func symlinkUnavailable(err error) bool {
	return errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) ||
		errors.Is(err, windowsErrorNotSupported)
}

func TestSymlinkUnavailableWindowsErrnos(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{name: "privilege not held", err: syscall.ERROR_PRIVILEGE_NOT_HELD},
		{name: "operation unsupported", err: windowsErrorNotSupported},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := &os.LinkError{Op: "symlink", Old: "target", New: "link", Err: tt.err}
			if !symlinkUnavailable(err) {
				t.Errorf("symlinkUnavailable(%v) = false, want true", err)
			}
		})
	}
}
