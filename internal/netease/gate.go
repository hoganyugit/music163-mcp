package netease

import (
	"sync"
	"time"
)

// Gate 让接口请求之间至少间隔 every，避免触发风控。
// lockPath 非空时（仅类 Unix 系统）通过文件锁在多个进程间共享这个间隔，
// 这样 MCP 服务和命令行批量调用加在一起也不会请求过密；文件锁不可用时退回到进程内间隔
type Gate struct {
	every    time.Duration
	lockPath string

	mu   sync.Mutex
	last time.Time
}

func NewGate(every time.Duration, lockPath string) *Gate {
	return &Gate{every: every, lockPath: lockPath}
}

// Wait 阻塞到距离上一次请求至少 every
func (g *Gate) Wait() {
	if g == nil || g.every <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lockPath != "" && g.waitShared() {
		g.last = time.Now()
		return
	}
	if d := g.every - time.Since(g.last); d > 0 {
		time.Sleep(d)
	}
	g.last = time.Now()
}

// pause 计算距离 last 还需要等多久；时钟回拨时最多等 every
func (g *Gate) pause(last time.Time) time.Duration {
	d := g.every - time.Since(last)
	return max(0, min(d, g.every))
}
