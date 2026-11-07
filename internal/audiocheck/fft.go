package audiocheck

import (
	"math"
	"math/bits"
)

// fftPlan 预计算 n 点基 2 FFT 的位反转表和旋转因子
type fftPlan struct {
	n    int
	rev  []int
	twid []complex128
}

func newFFTPlan(n int) *fftPlan {
	if n < 2 || n&(n-1) != 0 {
		panic("audiocheck: FFT 点数必须是 2 的幂")
	}
	p := &fftPlan{n: n, rev: make([]int, n), twid: make([]complex128, n/2)}
	shift := bits.UintSize - bits.TrailingZeros(uint(n))
	for i := range p.rev {
		p.rev[i] = int(bits.Reverse(uint(i)) >> shift)
	}
	for k := range p.twid {
		s, c := math.Sincos(-2 * math.Pi * float64(k) / float64(n))
		p.twid[k] = complex(c, s)
	}
	return p
}

// transform 原地计算 x 的离散傅里叶变换
func (p *fftPlan) transform(x []complex128) {
	n := p.n
	for i, j := range p.rev {
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		step := n / size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				w := p.twid[k*step]
				a := x[start+k]
				b := x[start+k+half] * w
				x[start+k] = a + b
				x[start+k+half] = a - b
			}
		}
	}
}

// hann 返回 n 点周期 Hann 窗，其系数和为 n/2
func hann(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
	}
	return w
}
