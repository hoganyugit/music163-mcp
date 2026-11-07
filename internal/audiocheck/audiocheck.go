// Package audiocheck 检测音频文件是否名副其实：完整性、试听片段、标称规格、
// 有损转码（假无损）、升频（伪 Hi-Res）和位深补零。全部用纯 Go 解码，不依赖 ffmpeg。
package audiocheck

import (
	"fmt"
	"math"
	"math/bits"
	"os"
	"time"
)

type Verdict string

const (
	VerdictGenuine      Verdict = "genuine"       // 未发现造假痕迹
	VerdictUpsampled    Verdict = "upsampled"     // 升频：高采样率里装的是低采样率音源
	VerdictPadded       Verdict = "padded"        // 位深补零：24bit 里装的是 16bit
	VerdictLossySuspect Verdict = "lossy_suspect" // 有一定有损转码嫌疑
	VerdictLossyLikely  Verdict = "lossy_likely"  // 大概率由有损音源转成
	VerdictTrial        Verdict = "trial"         // 试听片段
	VerdictCorrupt      Verdict = "corrupt"       // 损坏或截断
	VerdictMismatch     Verdict = "mismatch"      // 格式/规格与标称不符
	VerdictInconclusive Verdict = "inconclusive"  // 有效内容不足，无法判断
)

// 判定优先级：前面的问题更严重
var verdictOrder = []Verdict{
	VerdictCorrupt, VerdictTrial, VerdictMismatch, VerdictLossyLikely,
	VerdictLossySuspect, VerdictUpsampled, VerdictPadded,
}

// Expect 是调用方掌握的标称信息，未知的字段留空即可
type Expect struct {
	Format     string // 期望的容器格式 flac / mp3
	Level      string // 网易云音质档位
	DurationMs int    // 官方时长
	Trial      bool   // 接口已标明只给了试听片段
}

type Options struct {
	Expect      Expect
	Spectrogram string // 频谱图 PNG 输出路径，空则不生成
}

type Finding struct {
	Check  string `json:"check"`
	Status string `json:"status"` // pass / fail / warn / info
	Detail string `json:"detail"`
}

type BitStats struct {
	EffectiveBits     int     `json:"effective_bits"` // 去掉全零低位后的实际位深
	PeakDBFS          float64 `json:"peak_dbfs"`
	ClippedSamples    int64   `json:"clipped_samples"`
	IdenticalChannels bool    `json:"identical_channels"`
}

// Effective 是按检测结果折算的实际规格，用于在多个档位之间比较信息量
type Effective struct {
	Lossy    bool    `json:"lossy"`               // 有损音源（或本身就是有损格式）
	CutoffHz float64 `json:"cutoff_hz,omitempty"` // 有损音源的截止频率
	Rate     int     `json:"rate"`                // 实际采样率（升频时为推测的原始采样率）
	Bits     int     `json:"bits"`                // 实际位深，0 表示无法判断（升频后的低位不可信）
}

type Report struct {
	File          string    `json:"file"`
	Format        string    `json:"format"`
	SampleRate    int       `json:"sample_rate"`
	BitsPerSample int       `json:"bits_per_sample"`
	Channels      int       `json:"channels"`
	DurationSec   float64   `json:"duration_sec"`
	BitrateKbps   int       `json:"bitrate_kbps"`
	SizeBytes     int64     `json:"size_bytes"`
	Verdict       Verdict   `json:"verdict"`
	Confidence    string    `json:"confidence"` // high / medium / low
	Summary       string    `json:"summary"`
	EffectiveSpec string    `json:"effective_spec"`
	Effective     Effective `json:"effective"`
	Findings      []Finding `json:"findings"`
	Spectrum      *Spectrum `json:"spectrum,omitempty"`
	Bits          BitStats  `json:"bits"`
	Spectrogram   string    `json:"spectrogram,omitempty"`
	AnalysisMs    int64     `json:"analysis_ms"`
}

// Analyze 完整解码 path 并给出判定
func Analyze(path string, opt Options) (*Report, error) {
	start := time.Now()
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", path)
	}
	format, err := sniff(path)
	if err != nil {
		return nil, err
	}
	var d *decoded
	switch format {
	case "flac":
		d, err = decodeFLAC(path)
	case "mp3":
		d, err = decodeMP3(path)
	default:
		return nil, fmt.Errorf("暂不支持 %s 格式，只能检测 FLAC 和 MP3", format)
	}
	if err != nil {
		return nil, err
	}

	rep := &Report{
		File:          path,
		Format:        format,
		SampleRate:    d.rate,
		BitsPerSample: d.bits,
		Channels:      d.channels,
		SizeBytes:     st.Size(),
		DurationSec:   math.Round(float64(d.samples)/float64(d.rate)*100) / 100,
	}
	if rep.DurationSec > 0 {
		rep.BitrateKbps = int(float64(st.Size()) * 8 / rep.DurationSec / 1000)
	}
	var sp *Spectrum
	if mp := d.an.meanPower(); mp != nil {
		sp = newSpectrum(mp, d.rate, d.an.n, d.bits, d.an.used)
		rep.Spectrum = sp
	}
	rep.Bits = bitStats(d)
	judge(rep, d, sp, opt.Expect)
	if opt.Spectrogram != "" {
		if err := renderSpectrogram(opt.Spectrogram, d, sp, rep); err != nil {
			rep.Findings = append(rep.Findings, Finding{Check: "spectrogram", Status: "info", Detail: "频谱图生成失败: " + err.Error()})
		} else {
			rep.Spectrogram = opt.Spectrogram
		}
	}
	rep.AnalysisMs = time.Since(start).Milliseconds()
	return rep, nil
}

func bitStats(d *decoded) BitStats {
	a := d.an
	bs := BitStats{ClippedSamples: a.clipped, PeakDBFS: -200}
	if a.orBits != 0 {
		bs.EffectiveBits = d.bits - bits.TrailingZeros32(a.orBits)
	}
	if a.peak > 0 {
		bs.PeakDBFS = round1(20 * math.Log10(float64(a.peak)/float64(int64(1)<<(d.bits-1))))
	}
	bs.IdenticalChannels = d.channels >= 2 && d.samples > 0 && a.diffLR == 0
	return bs
}

type judgement struct {
	rep     *Report
	found   map[Verdict]string // 每种问题的说明
	conf    map[Verdict]string
	lossy   bool
	upRate  int // 升频时推测的原始采样率
	padded  bool
	cutoffs float64
}

func (j *judgement) add(check, status, detail string) {
	j.rep.Findings = append(j.rep.Findings, Finding{Check: check, Status: status, Detail: detail})
}

func (j *judgement) flag(v Verdict, conf, detail string) {
	if _, ok := j.found[v]; ok {
		return
	}
	j.found[v] = detail
	j.conf[v] = conf
}

func judge(rep *Report, d *decoded, sp *Spectrum, exp Expect) {
	j := &judgement{rep: rep, found: map[Verdict]string{}, conf: map[Verdict]string{}}
	dur := float64(d.samples) / float64(d.rate)

	// 完整性
	if d.decodeErr != "" {
		msg := "解码中途出错，文件损坏或不完整: " + d.decodeErr
		j.add("integrity", "fail", msg)
		j.flag(VerdictCorrupt, "high", msg)
	} else if d.declared > 0 && d.samples < d.declared {
		msg := fmt.Sprintf("文件被截断：文件头声明 %s，实际只解出 %s", fmtDur(float64(d.declared)/float64(d.rate)), fmtDur(dur))
		j.add("integrity", "fail", msg)
		j.flag(VerdictCorrupt, "high", msg)
	}
	switch d.md5Status {
	case "ok":
		j.add("integrity", "pass", "FLAC 内置的音频 MD5 校验通过，解码结果逐位正确")
	case "mismatch":
		msg := "解码结果与 FLAC 内置的音频 MD5 不符，文件已损坏"
		j.add("integrity", "fail", msg)
		j.flag(VerdictCorrupt, "high", msg)
	case "absent":
		j.add("integrity", "info", "文件没有记录音频 MD5，无法自校验")
	}

	// 时长与试听
	if exp.Trial {
		msg := "接口标明只给了试听片段"
		j.add("trial", "fail", msg)
		j.flag(VerdictTrial, "high", msg)
	}
	if exp.DurationMs > 0 {
		want := float64(exp.DurationMs) / 1000
		switch {
		case dur < want*0.9 && want-dur > 10:
			msg := fmt.Sprintf("时长只有 %s，完整歌曲是 %s，这是片段", fmtDur(dur), fmtDur(want))
			j.add("duration", "fail", msg)
			j.flag(VerdictTrial, "high", msg)
		case math.Abs(dur-want) > 10:
			j.add("duration", "warn", fmt.Sprintf("时长 %s，与官方时长 %s 相差较多", fmtDur(dur), fmtDur(want)))
		default:
			j.add("duration", "pass", fmt.Sprintf("时长 %s，与官方时长一致", fmtDur(dur)))
		}
	}

	// 标称规格
	if exp.Format != "" && exp.Format != rep.Format {
		msg := fmt.Sprintf("标称 %s，实际文件是 %s", exp.Format, rep.Format)
		j.add("format", "fail", msg)
		j.flag(VerdictMismatch, "high", msg)
	}
	switch exp.Level {
	case "lossless", "hires", "jymaster":
		if rep.Format == "mp3" && exp.Format == "" {
			msg := fmt.Sprintf("标称%s，实际是 MP3", levelName(exp.Level))
			j.add("format", "fail", msg)
			j.flag(VerdictMismatch, "high", msg)
		}
		if (exp.Level == "hires" || exp.Level == "jymaster") && rep.Format == "flac" && d.rate <= 48000 && d.bits <= 16 {
			msg := fmt.Sprintf("标称%s，实际只有 %dbit/%s", levelName(exp.Level), d.bits, rateStr(d.rate))
			j.add("format", "fail", msg)
			j.flag(VerdictMismatch, "high", msg)
		}
	}

	if sp == nil {
		j.add("spectrum", "info", "有效音频太短或几乎全是静音，无法做频谱分析")
	} else if rep.Format == "mp3" {
		j.judgeMP3Spectrum(sp)
	} else {
		j.judgeLossless(sp, d.rate)
	}
	j.judgeBits(d)
	if rep.Bits.IdenticalChannels {
		j.add("channels", "info", "左右声道完全相同，实际是单声道")
	}
	if total := d.samples * int64(d.channels); total > 0 && rep.Bits.ClippedSamples*1000 > total {
		j.add("clipping", "info", fmt.Sprintf("有 %.2f%% 的采样顶到满幅，削波较多（母带压得很响）", float64(rep.Bits.ClippedSamples)*100/float64(total)))
	}

	// 主判定
	rep.Verdict = ""
	for _, v := range verdictOrder {
		if _, ok := j.found[v]; ok {
			rep.Verdict, rep.Confidence = v, j.conf[v]
			break
		}
	}
	if rep.Verdict == "" {
		if sp == nil {
			rep.Verdict, rep.Confidence = VerdictInconclusive, "low"
		} else {
			rep.Verdict, rep.Confidence = VerdictGenuine, "medium"
		}
	}

	eff := Effective{Rate: d.rate, Bits: d.bits}
	if j.padded {
		eff.Bits = rep.Bits.EffectiveBits
	}
	if j.upRate > 0 {
		eff.Rate, eff.Bits = j.upRate, 0
	}
	if j.lossy {
		eff.Lossy, eff.CutoffHz = true, j.cutoffs
	}
	rep.Effective = eff
	rep.EffectiveSpec = effectiveSpec(rep, eff)
	rep.Summary = j.summary(sp)
}

func (j *judgement) judgeLossless(sp *Spectrum, rate int) {
	strong, weak := sp.strongCliff(), sp.weakCliff()
	fc, drop := sp.CliffHz, sp.CliffDropDB
	switch {
	case strong && fc < 19600:
		conf := "medium"
		if drop >= 35 && fc < 19000 {
			conf = "high"
		}
		msg := fmt.Sprintf("频谱在 %.1f kHz 处陡降 %.0f dB，上方几乎没有内容，符合有损编码的低通特征（%s）", fc/1000, drop, lossyGuess(fc))
		j.add("lossy", "fail", msg)
		j.flag(VerdictLossyLikely, conf, msg)
		j.lossy, j.cutoffs = true, fc
	case strong && fc < 20600:
		// 48k 及以上的母带很少在 20 kHz 附近做低通，嫌疑更大
		conf := "low"
		if rate >= 48000 {
			conf = "medium"
		}
		msg := fmt.Sprintf("频谱在 %.1f kHz 处陡降 %.0f dB。可能来自 256–320k 的有损音源，也可能只是母带做了低通，单看频谱无法确定", fc/1000, drop)
		j.add("lossy", "warn", msg)
		j.flag(VerdictLossySuspect, conf, msg)
		j.lossy, j.cutoffs = true, fc
	case weak && fc < 19000:
		msg := fmt.Sprintf("频谱在 %.1f kHz 附近下降了 %.0f dB，有一定有损转码嫌疑，但不够明显", fc/1000, drop)
		j.add("lossy", "warn", msg)
		j.flag(VerdictLossySuspect, "low", msg)
		j.lossy, j.cutoffs = true, fc
	default:
		j.add("lossy", "pass", fmt.Sprintf("没有发现有损编码的低通断崖，有效带宽约 %.1f kHz", sp.BandwidthHz/1000))
	}

	if rate <= 50000 {
		return
	}
	switch {
	case (strong || weak) && fc <= 24600:
		conf := "medium"
		if strong {
			conf = "high"
		}
		src := 44100
		if fc > 22600 {
			src = 48000
		}
		msg := fmt.Sprintf("标称 %s，但频谱在 %.1f kHz 处截止，上方没有真实内容，实际是 %s 音源升频", rateStr(rate), fc/1000, rateStr(src))
		j.add("upsample", "fail", msg)
		j.flag(VerdictUpsampled, conf, msg)
		j.upRate = src
	case strong && rate > 100000 && fc >= 38000 && fc <= 48600:
		src := 88200
		if fc > 44600 {
			src = 96000
		}
		msg := fmt.Sprintf("标称 %s，但频谱在 %.1f kHz 处截止，实际是 %s 音源升频", rateStr(rate), fc/1000, rateStr(src))
		j.add("upsample", "fail", msg)
		j.flag(VerdictUpsampled, "high", msg)
		j.upRate = src
	case sp.UltrasonicDB != nil && *sp.UltrasonicDB < 10:
		msg := fmt.Sprintf("24.5 kHz 以上的电平只比量化噪底高 %.0f dB，基本是空的，实际不超过 48kHz 规格", *sp.UltrasonicDB)
		j.add("upsample", "fail", msg)
		j.flag(VerdictUpsampled, "medium", msg)
		j.upRate = 48000
	case sp.UltrasonicDB != nil:
		j.add("upsample", "pass", fmt.Sprintf("24.5 kHz 以上有实际内容（比量化噪底高 %.0f dB），不是简单升频。注意：算法补出来的高频也能骗过这一项", *sp.UltrasonicDB))
	}
}

func (j *judgement) judgeMP3Spectrum(sp *Spectrum) {
	cut := sp.BandwidthHz
	if sp.strongCliff() {
		cut = sp.CliffHz
	}
	j.lossy, j.cutoffs = true, cut
	if j.rep.BitrateKbps >= 250 && sp.strongCliff() && sp.CliffHz < 18500 {
		msg := fmt.Sprintf("这是 %d kbps 的 MP3，但频谱在 %.1f kHz 就截止了，更像是由低码率文件转成的", j.rep.BitrateKbps, sp.CliffHz/1000)
		j.add("lossy", "warn", msg)
		j.flag(VerdictLossySuspect, "medium", msg)
		return
	}
	j.add("lossy", "info", fmt.Sprintf("MP3 本身是有损格式，频谱截止约 %.1f kHz，与码率相符", cut/1000))
}

func (j *judgement) judgeBits(d *decoded) {
	eff := j.rep.Bits.EffectiveBits
	switch {
	case eff == 0:
	case d.bits > 16 && eff <= 16:
		msg := fmt.Sprintf("标称 %dbit，但所有采样的低 %d 位全是 0，实际只有 %dbit", d.bits, d.bits-eff, eff)
		j.add("bitdepth", "fail", msg)
		j.flag(VerdictPadded, "high", msg)
		j.padded = true
	case d.bits > 16:
		j.add("bitdepth", "pass", "低位有实际数据，不是直接补零（加过抖动再转 24bit 的查不出来）")
	case eff < d.bits-3:
		j.add("bitdepth", "warn", fmt.Sprintf("标称 %dbit，实际只用到 %dbit", d.bits, eff))
	}
}

func (j *judgement) summary(sp *Spectrum) string {
	rep := j.rep
	label := map[Verdict]string{
		VerdictCorrupt:      "文件损坏",
		VerdictTrial:        "试听片段",
		VerdictMismatch:     "规格与标称不符",
		VerdictLossyLikely:  "大概率是有损转码（假无损）",
		VerdictLossySuspect: "有点可疑",
		VerdictUpsampled:    "升频（伪 Hi-Res）",
		VerdictPadded:       "位深补零（伪 24bit）",
	}
	if l, ok := label[rep.Verdict]; ok {
		return l + "：" + j.found[rep.Verdict]
	}
	spec := fmt.Sprintf("%dbit/%s", rep.BitsPerSample, rateStr(rep.SampleRate))
	switch {
	case rep.Verdict == VerdictInconclusive:
		return "无法判断：有效音频太少，做不了频谱分析"
	case rep.Format == "mp3":
		return fmt.Sprintf("有损 MP3（约 %d kbps），频谱截止约 %.1f kHz，与码率相符", rep.BitrateKbps, j.cutoffs/1000)
	case rep.SampleRate > 50000:
		return fmt.Sprintf("真 Hi-Res：%s，24.5 kHz 以上有实际内容，没有发现有损转码、升频或补零的痕迹", spec)
	default:
		bw := 0.0
		if sp != nil {
			bw = sp.BandwidthHz / 1000
		}
		return fmt.Sprintf("未发现造假痕迹：%s，频谱一直延伸到约 %.1f kHz，没有有损编码的低通断崖", spec, bw)
	}
}

func effectiveSpec(rep *Report, eff Effective) string {
	switch {
	case rep.Format == "mp3":
		return fmt.Sprintf("有损 MP3 约 %d kbps", rep.BitrateKbps)
	case eff.Lossy:
		return fmt.Sprintf("≈有损音源（%s，截止 %.1f kHz）", lossyGuess(eff.CutoffHz), eff.CutoffHz/1000)
	case eff.Rate != rep.SampleRate:
		return fmt.Sprintf("≈%s 音源（升频到 %s）", rateStr(eff.Rate), rateStr(rep.SampleRate))
	case eff.Bits != rep.BitsPerSample:
		return fmt.Sprintf("%dbit/%s（标称 %dbit）", eff.Bits, rateStr(rep.SampleRate), rep.BitsPerSample)
	default:
		return fmt.Sprintf("%dbit/%s", rep.BitsPerSample, rateStr(rep.SampleRate))
	}
}

func lossyGuess(fc float64) string {
	switch {
	case fc < 15000:
		return "约 96k 或更低码率"
	case fc < 17600:
		return "约 128k 码率级别"
	case fc < 18700:
		return "约 160–192k 码率级别"
	case fc < 19600:
		return "约 192–256k 码率级别"
	default:
		return "约 256–320k 码率级别"
	}
}

func rateStr(rate int) string {
	if rate%1000 == 0 {
		return fmt.Sprintf("%dkHz", rate/1000)
	}
	return fmt.Sprintf("%.1fkHz", float64(rate)/1000)
}

func fmtDur(sec float64) string {
	s := int(math.Round(sec))
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func levelName(level string) string {
	switch level {
	case "standard":
		return "标准"
	case "higher":
		return "较高"
	case "exhigh":
		return "极高"
	case "lossless":
		return "无损"
	case "hires":
		return "Hi-Res"
	case "jymaster":
		return "超清母带"
	}
	return level
}
