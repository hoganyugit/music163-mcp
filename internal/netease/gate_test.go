package netease

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestGateSpacesRequests(t *testing.T) {
	g := NewGate(50*time.Millisecond, "")
	start := time.Now()
	for range 3 {
		g.Wait()
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("3 waits took %s, want >= 100ms", d)
	}
}

// 两个 Gate 共用一个锁文件，模拟两个进程：交替请求也要保持间隔
func TestGateSharedAcrossInstances(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("文件锁只在类 Unix 系统上启用")
	}
	lock := filepath.Join(t.TempDir(), "api.lock")
	a, b := NewGate(50*time.Millisecond, lock), NewGate(50*time.Millisecond, lock)
	start := time.Now()
	a.Wait()
	b.Wait()
	a.Wait()
	b.Wait()
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("4 alternating waits took %s, want >= 150ms", d)
	}
}

func TestNilGate(t *testing.T) {
	var g *Gate
	g.Wait()
}
