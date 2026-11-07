//go:build unix

package netease

import (
	"encoding/binary"
	"os"
	"syscall"
	"time"
)

// waitShared 持有文件锁时读出所有进程中最近一次请求的时间，等够间隔后写回当前时间
func (g *Gate) waitShared() bool {
	f, err := os.OpenFile(g.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	var buf [8]byte
	if n, _ := f.ReadAt(buf[:], 0); n == len(buf) {
		time.Sleep(g.pause(time.Unix(0, int64(binary.BigEndian.Uint64(buf[:])))))
	}
	binary.BigEndian.PutUint64(buf[:], uint64(time.Now().UnixNano()))
	_, err = f.WriteAt(buf[:], 0)
	return err == nil
}
