package acquire

import (
	"math"

	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/netease"
)

// rateClass 把采样率归为 CD 级（≤48k）、双倍（≤96k）、四倍（≤192k）及以上
func rateClass(rate int) int {
	switch {
	case rate <= 50000:
		return 1
	case rate <= 100000:
		return 2
	case rate <= 200000:
		return 3
	}
	return 4
}

// compareEffective 比较两个档位按实测折算后的信息量：>0 表示 a 更多，0 表示相当，<0 表示更少
func compareEffective(a, b audiocheck.Effective) int {
	if a.Lossy != b.Lossy {
		if !a.Lossy {
			return 1
		}
		return -1
	}
	if a.Lossy {
		d := a.CutoffHz - b.CutoffHz
		switch {
		case math.Abs(d) <= 500:
			return 0
		case d > 0:
			return 1
		}
		return -1
	}
	if ca, cb := rateClass(a.Rate), rateClass(b.Rate); ca != cb {
		if ca > cb {
			return 1
		}
		return -1
	}
	// 位深为 0 表示无法判断（升频后的低位不可信），视为相当
	if a.Bits != 0 && b.Bits != 0 && a.Bits != b.Bits {
		if a.Bits > b.Bits {
			return 1
		}
		return -1
	}
	return 0
}

// canStillWin 判断更低的档位是否还可能不输给当前最佳结果；不可能时就不必再下载
func canStillWin(level string, best audiocheck.Effective, bestFormat string) bool {
	switch level {
	case netease.LevelExhigh, netease.LevelHigher, netease.LevelStandard:
		// 已经有 FLAC 就不再降到 MP3
		return bestFormat != "flac"
	case netease.LevelLossless:
		// 无损档是 CD 规格，比不过实测高于 48kHz 的真高解析
		return best.Lossy || rateClass(best.Rate) <= 1
	}
	return true
}
