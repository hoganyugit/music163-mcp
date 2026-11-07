//go:build !unix

package acquire

import "os"

func isMountPoint(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func freeBytes(string) (uint64, error) { return 0, os.ErrInvalid }
