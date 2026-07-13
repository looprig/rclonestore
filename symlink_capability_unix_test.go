//go:build !windows

package rclonestore

import (
	"errors"
	"io/fs"
)

func symlinkUnavailable(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}
