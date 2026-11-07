package audiocheck

import (
	"math"
)

const bandHz = 100.0 // 平均频谱按 100 Hz 分带

// Spectrum 是整首歌平均频谱的关键指标；电平单位均为 dB（满幅正弦约 0 dB）
type Spectrum struct {
	FloorDB float64 `json:"quant_floor_db"` // 标称位深下量化噪声在单个频点的理论电平
	// 最陡的一处"断崖"：中心频率、断崖上下 1 kHz 内的电平落差、断崖下方内容比噪底高多少
	CliffHz        float64 `json:"cliff_hz,omitempty"`
	CliffDropDB    float64 `json:"cliff_drop_db,omitempty"`
	CliffContentDB float64 `json:"cliff_content_db,omitempty"`
	BandwidthHz    float64 `json:"bandwidth_hz"` // 有效带宽：平滑后仍明显高于底噪的最高频率
	// 24.5 kHz 以上（最高到 40 kHz）的平均电平比量化噪底高多少，仅高采样率文件有此项
	UltrasonicDB *float64 `json:"ultrasonic_above_floor_db,omitempty"`
	Windows      int      `json:"windows"`

	levels []float64 // 每 100 Hz 一个频带的平均电平
	nyq    float64
}

func newSpectrum(meanPow []float64, rate, n, bits, windows int) *Spectrum {
	nyq := float64(rate) / 2
	sp := &Spectrum{FloorDB: quantFloorDB(bits, n), Windows: windows, nyq: nyq}
	binHz := float64(rate) / float64(n)
	nb := int(nyq / bandHz)
	sp.levels = make([]float64, nb)
	for b := range sp.levels {
		k0 := int(math.Ceil(float64(b) * bandHz / binHz))
		k1 := int(math.Ceil(float64(b+1) * bandHz / binHz))
		k1 = min(k1, len(meanPow))
		if k1 <= k0 {
			k1 = min(k0+1, len(meanPow))
			k0 = k1 - 1
		}
		var s float64
		for k := k0; k < k1; k++ {
			s += meanPow[k]
		}
		sp.levels[b] = 10 * math.Log10(s/float64(k1-k0)+1e-30)
	}
	sp.findCliff(rate)
	sp.findBandwidth()
	if nyq > 30000 {
		if u := sp.meanLevel(24500, min(40000, nyq-2000)) - sp.FloorDB; !math.IsNaN(u) {
			u = round1(u)
			sp.UltrasonicDB = &u
		}
	}
	return sp
}

// meanLevel 返回完全落在 [f0, f1) 内的频带电平的平均值（dB 域平均，不易被个别强频带带偏）
func (sp *Spectrum) meanLevel(f0, f1 float64) float64 {
	var s float64
	var cnt int
	for b, l := range sp.levels {
		lo, hi := float64(b)*bandHz, float64(b+1)*bandHz
		if lo >= f0 && hi <= f1 {
			s += l
			cnt++
		}
	}
	if cnt == 0 {
		return math.NaN()
	}
	return s / float64(cnt)
}

// findCliff 在 11 kHz 以上找电平落差最大的位置：有损编码的低通、升频的抗混叠滤波都会留下这种断崖
func (sp *Spectrum) findCliff(rate int) {
	lo, hi := 11000.0, sp.nyq-1000
	if rate > 50000 {
		hi = math.Min(sp.nyq*0.9, sp.nyq-2000)
	}
	var bestDrop, bestF, bestBelow float64
	for f := lo; f <= hi; f += bandHz {
		below := sp.meanLevel(f-1000, f-200)
		above := sp.meanLevel(f+200, f+1000)
		if math.IsNaN(below) || math.IsNaN(above) {
			continue
		}
		if d := below - above; d > bestDrop {
			bestDrop, bestF, bestBelow = d, f, below
		}
	}
	if bestDrop < 10 {
		return
	}
	sp.CliffHz = bestF
	sp.CliffDropDB = round1(bestDrop)
	sp.CliffContentDB = round1(bestBelow - sp.FloorDB)
}

// findBandwidth 以平滑后最安静的频段为底噪，有效带宽是仍比它高 10 dB 的最高频率
func (sp *Spectrum) findBandwidth() {
	smooth := make([]float64, len(sp.levels))
	for b := range sp.levels {
		lo, hi := max(0, b-2), min(len(sp.levels)-1, b+2)
		var s float64
		for i := lo; i <= hi; i++ {
			s += sp.levels[i]
		}
		smooth[b] = s / float64(hi-lo+1)
	}
	floorEst := math.Inf(1)
	for b, l := range smooth {
		if f := (float64(b) + 0.5) * bandHz; f >= 1000 && f <= sp.nyq-300 {
			floorEst = math.Min(floorEst, l)
		}
	}
	if math.IsInf(floorEst, 1) {
		return
	}
	thr := math.Max(floorEst, sp.FloorDB) + 10
	for b := len(smooth) - 1; b >= 0; b-- {
		if smooth[b] >= thr {
			sp.BandwidthHz = float64(b+1) * bandHz
			return
		}
	}
}

// strongCliff 有损低通或重采样滤波：1 kHz 内掉 25 dB 以上，且断崖下方确有内容
func (sp *Spectrum) strongCliff() bool {
	return sp.CliffHz > 0 && sp.CliffDropDB >= 25 && sp.CliffContentDB >= 20
}

func (sp *Spectrum) weakCliff() bool {
	return sp.CliffHz > 0 && !sp.strongCliff() && sp.CliffDropDB >= 15 && sp.CliffContentDB >= 15
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
