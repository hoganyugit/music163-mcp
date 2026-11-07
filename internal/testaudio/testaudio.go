// Package testaudio 为测试合成"像音乐"的音频并写成 FLAC，只在测试中使用
package testaudio

import (
	"math"
	"math/cmplx"
	"math/rand"
	"os"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// MusicLevel 近似真实音乐的平均频谱：粉红噪声斜率，10 kHz 以上额外滚降（dB）
func MusicLevel(f float64) float64 {
	return -3*math.Log2(math.Max(f, 100)/100) - 12*math.Max(0, math.Log2(f/10000))
}

// Lowpass 在 fc 处加一段 width 宽的过渡带，之上完全没有内容（模拟有损编码低通或重采样滤波器）
func Lowpass(base func(float64) float64, fc, width float64) func(float64) float64 {
	return func(f float64) float64 {
		switch {
		case f < fc:
			return base(f)
		case f < fc+width:
			return base(f) - 120*(f-fc)/width
		default:
			return -400
		}
	}
}

// CDMaster 模拟 CD：重采样滤波器在 21.6 kHz 附近
func CDMaster(f float64) float64 { return Lowpass(MusicLevel, 21600, 400)(f) }

// HiresReal 模拟真高解析：内容一直延伸到超声波段
func HiresReal(f float64) float64 { return math.Max(MusicLevel(f), -95) }

// Synth 在频域按电平曲线合成随机相位噪声，逐块随机改变响度模拟动态，返回 [-1,1] 浮点立体声
func Synth(rate int, seconds float64, level func(float64) float64, seed int64) [2][]float64 {
	const n = 16384
	hop := n / 2
	total := int(seconds * float64(rate))
	var out [2][]float64
	for c := range out {
		out[c] = make([]float64, total+n)
	}
	win := make([]float64, n)
	for i := range win {
		win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/n)
	}
	rng := rand.New(rand.NewSource(seed))
	mag := make([]float64, n/2+1)
	for k := range mag {
		mag[k] = math.Pow(10, level(float64(k)*float64(rate)/n)/20)
	}
	buf := make([]complex128, n)
	gain := 0.6
	for start := 0; start < total; start += hop {
		gain = math.Max(0.15, math.Min(1, gain+rng.NormFloat64()*0.1))
		for c := 0; c < 2; c++ {
			for k := 1; k < n/2; k++ {
				buf[k] = cmplx.Rect(mag[k], rng.Float64()*2*math.Pi)
				buf[n-k] = cmplx.Conj(buf[k])
			}
			buf[0], buf[n/2] = 0, 0
			ifft(buf)
			for i := 0; i < n; i++ {
				out[c][start+i] += real(buf[i]) * win[i] * gain
			}
		}
	}
	var peak float64
	for c := range out {
		out[c] = out[c][:total]
		for _, v := range out[c] {
			peak = math.Max(peak, math.Abs(v))
		}
	}
	for c := range out {
		for i := range out[c] {
			out[c][i] *= 0.9 / peak
		}
	}
	return out
}

// ifft 原地逆变换（未除以 n，调用方会重新归一化）
func ifft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		step := cmplx.Exp(complex(0, 2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < size/2; k++ {
				a, b := x[start+k], x[start+k+size/2]*w
				x[start+k], x[start+k+size/2] = a+b, a-b
				w *= step
			}
		}
	}
}

// Quantize 加 TPDF 抖动后量化到 bits 位
func Quantize(x [2][]float64, bits int, seed int64) [2][]int32 {
	rng := rand.New(rand.NewSource(seed))
	full := float64(int64(1) << (bits - 1))
	var out [2][]int32
	for c := range x {
		out[c] = make([]int32, len(x[c]))
		for i, v := range x[c] {
			s := math.Round(v*full + rng.Float64() - rng.Float64())
			out[c][i] = int32(math.Max(-full, math.Min(full-1, s)))
		}
	}
	return out
}

// Pad16To24 把 16bit 采样左移 8 位存成 24bit（位深补零）
func Pad16To24(pcm [2][]int32) [2][]int32 {
	for c := range pcm {
		for i := range pcm[c] {
			pcm[c][i] <<= 8
		}
	}
	return pcm
}

// WriteFLAC 用 mewkiz/flac 编码器写出立体声 FLAC（含正确的音频 MD5）
func WriteFLAC(path string, pcm [2][]int32, rate, bits int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	info := &meta.StreamInfo{BlockSizeMin: 4096, BlockSizeMax: 4096, SampleRate: uint32(rate), NChannels: 2, BitsPerSample: uint8(bits)}
	enc, err := flac.NewEncoder(f, info)
	if err != nil {
		_ = f.Close()
		return err
	}
	for start := 0; start < len(pcm[0]); start += 4096 {
		end := min(start+4096, len(pcm[0]))
		fr := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(end - start),
			SampleRate:        uint32(rate),
			Channels:          frame.ChannelsLR,
			BitsPerSample:     uint8(bits),
		}}
		for c := 0; c < 2; c++ {
			s := pcm[c][start:end]
			fr.Subframes = append(fr.Subframes, &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: s, NSamples: len(s)})
		}
		if err := enc.WriteFrame(fr); err != nil {
			_ = enc.Close()
			return err
		}
	}
	return enc.Close()
}

// MakeFLAC 合成并写出一个测试文件
func MakeFLAC(path string, rate, bits int, seconds float64, level func(float64) float64, padded bool) error {
	x := Synth(rate, seconds, level, 1)
	var pcm [2][]int32
	if padded {
		pcm = Pad16To24(Quantize(x, 16, 2))
	} else {
		pcm = Quantize(x, bits, 2)
	}
	return WriteFLAC(path, pcm, rate, bits)
}
