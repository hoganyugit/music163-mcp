//go:build unix

package acquire

import (
	"os"
	"path/filepath"
	"syscall"
)

// isMountPoint 判断 path 是否是一个已挂载文件系统的根（与上级目录不在同一设备上）
func isMountPoint(path string) bool {
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return false
	}
	if path == "/" {
		return true
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false
	}
	a, ok1 := st.Sys().(*syscall.Stat_t)
	b, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return uint64(a.Dev) != uint64(b.Dev)
}

// freeBytes 返回 path 所在文件系统的可用空间
func freeBytes(path string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), nil
}
