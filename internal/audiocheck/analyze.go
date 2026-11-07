package audiocheck

import (
	"math"
)

const (
	maxWindows = 720  // 最多分析的窗口数（也是频谱图的列数上限）
	specRows   = 400  // 频谱图纵向分辨率
	silenceRMS = 1e-4 // 窗口 RMS 低于约 -80 dBFS 视为静音，不计入平均频谱
)

type window struct {
	start int64
	fill  int
	data  [][]float64
}

// analyzer 流式接收解码后的 PCM：在整首歌里均匀取窗做 FFT，同时统计位深、峰值、声道差异
type analyzer struct {
	rate     int
	channels int
	bits     int
	n        int
	plan     *fftPlan
	win      []float64
	scale    float64

	planned []int64 // 预先规划的窗口起点；nil 表示按 n 连续取窗
	next    int
	nextPos int64
	active  []*window
	free    []*window
	pos     int64

	zbuf   []complex128
	pow    []float64
	sumPow []float64
	used   int
	cols   [][]float32
	silent []bool

	orBits  uint32
	peak    int64
	diffLR  int64
	clipped int64
}

// fftSize 让频率分辨率保持在约 6–12 Hz
func fftSize(rate int) int {
	switch {
	case rate <= 96000:
		return 8192
	case rate <= 192000:
		return 16384
	default:
		return 32768
	}
}

// total 为每声道总采样数，未知时传 0
func newAnalyzer(rate, channels, bits int, total int64) *analyzer {
	n := fftSize(rate)
	a := &analyzer{
		rate:     rate,
		channels: channels,
		bits:     bits,
		n:        n,
		plan:     newFFTPlan(n),
		win:      hann(n),
		scale:    1 / float64(int64(1)<<(bits-1)),
		zbuf:     make([]complex128, n),
		pow:      make([]float64, n/2+1),
		sumPow:   make([]float64, n/2+1),
	}
	if total >= int64(n) {
		k := int((total-int64(n))/int64(n/4)) + 1
		k = min(k, maxWindows)
		a.planned = make([]int64, k)
		for i := range a.planned {
			if k > 1 {
				a.planned[i] = int64(i) * (total - int64(n)) / int64(k-1)
			}
		}
	}
	return a
}

func (a *analyzer) peekStart() (int64, bool) {
	if a.planned != nil {
		if a.next < len(a.planned) {
			return a.planned[a.next], true
		}
		return 0, false
	}
	return a.nextPos, true
}

func (a *analyzer) popStart() {
	if a.planned != nil {
		a.next++
	} else {
		a.nextPos += int64(a.n)
	}
}

func (a *analyzer) getWindow(start int64) *window {
	var w *window
	if k := len(a.free); k > 0 {
		w = a.free[k-1]
		a.free = a.free[:k-1]
	} else {
		w = &window{data: make([][]float64, a.channels)}
		for c := range w.data {
			w.data[c] = make([]float64, a.n)
		}
	}
	w.start, w.fill = start, 0
	return w
}

// feed 接收一块各声道等长的整数采样
func (a *analyzer) feed(block [][]int32) {
	if len(block) < a.channels || len(block[0]) == 0 {
		return
	}
	m := len(block[0])
	a.stats(block)

	f0, f1 := a.pos, a.pos+int64(m)
	for {
		s, ok := a.peekStart()
		if !ok || s >= f1 {
			break
		}
		a.popStart()
		if s >= f0 {
			a.active = append(a.active, a.getWindow(s))
		}
	}
	kept := a.active[:0]
	for _, w := range a.active {
		from := max(f0, w.start+int64(w.fill))
		to := min(f1, w.start+int64(a.n))
		if from < to {
			off := int(from - w.start)
			for c := 0; c < a.channels; c++ {
				src := block[c][from-f0 : to-f0]
				dst := w.data[c][off : off+len(src)]
				for i, s := range src {
					dst[i] = float64(s) * a.scale
				}
			}
			w.fill = int(to - w.start)
		}
		if w.fill == a.n {
			a.process(w)
			a.free = append(a.free, w)
		} else {
			kept = append(kept, w)
		}
	}
	a.active = kept
	a.pos = f1
}

func (a *analyzer) stats(block [][]int32) {
	maxVal := int64(1)<<(a.bits-1) - 1
	for c := 0; c < a.channels; c++ {
		for _, s := range block[c] {
			a.orBits |= uint32(s)
			v := int64(s)
			if v < 0 {
				v = -v
			}
			if v > a.peak {
				a.peak = v
			}
			if v >= maxVal {
				a.clipped++
			}
		}
	}
	if a.channels >= 2 {
		l, r := block[0], block[1]
		for i := range l {
			if l[i] != r[i] {
				a.diffLR++
			}
		}
	}
}

// process 对一个窗口做 FFT：两路实信号打包成一次复数 FFT，功率按声道平均
// 归一化后满幅正弦在其频点约为 0 dB
func (a *analyzer) process(w *window) {
	var energy float64
	for c := 0; c < a.channels; c++ {
		for _, v := range w.data[c] {
			energy += v * v
		}
	}
	isSilent := math.Sqrt(energy/float64(a.n*a.channels)) < silenceRMS

	n, half := a.n, a.n/2
	pow := a.pow
	clear(pow)
	z := a.zbuf
	for c := 0; c < a.channels; c += 2 {
		if c+1 < a.channels {
			x, y := w.data[c], w.data[c+1]
			for i := range z {
				z[i] = complex(x[i]*a.win[i], y[i]*a.win[i])
			}
			a.plan.transform(z)
			for k := 0; k <= half; k++ {
				zk, zn := z[k], z[(n-k)&(n-1)]
				xr, xi := (real(zk)+real(zn))/2, (imag(zk)-imag(zn))/2
				yr, yi := (imag(zk)+imag(zn))/2, (real(zn)-real(zk))/2
				pow[k] += xr*xr + xi*xi + yr*yr + yi*yi
			}
		} else {
			x := w.data[c]
			for i := range z {
				z[i] = complex(x[i]*a.win[i], 0)
			}
			a.plan.transform(z)
			for k := 0; k <= half; k++ {
				pow[k] += real(z[k])*real(z[k]) + imag(z[k])*imag(z[k])
			}
		}
	}
	norm := 16 / (float64(n) * float64(n) * float64(a.channels))
	for k := range pow {
		pow[k] *= norm
	}
	if !isSilent {
		for k, p := range pow {
			a.sumPow[k] += p
		}
		a.used++
	}
	col := make([]float32, specRows)
	for r := range col {
		k0, k1 := r*half/specRows, (r+1)*half/specRows
		if k1 <= k0 {
			k1 = k0 + 1
		}
		var s float64
		for k := k0; k < k1; k++ {
			s += pow[k]
		}
		col[r] = float32(s / float64(k1-k0))
	}
	a.cols = append(a.cols, col)
	a.silent = append(a.silent, isSilent)
}

// meanPower 返回非静音窗口的平均功率谱；没有可用窗口时返回 nil
func (a *analyzer) meanPower() []float64 {
	if a.used == 0 {
		return nil
	}
	out := make([]float64, len(a.sumPow))
	for k, p := range a.sumPow {
		out[k] = p / float64(a.used)
	}
	return out
}

// quantFloorDB 给定位深和 FFT 点数，返回白色量化噪声在单个频点上的期望电平
func quantFloorDB(bits, n int) float64 {
	q := math.Ldexp(1, 1-bits)
	sigma2 := q * q / 12
	return 10 * math.Log10(6*sigma2/float64(n))
}
