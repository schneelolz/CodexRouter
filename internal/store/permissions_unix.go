//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

func validateDataDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("数据目录必须属于当前用户且权限为 0700；请显式选择私有目录")
	}
	return nil
}
