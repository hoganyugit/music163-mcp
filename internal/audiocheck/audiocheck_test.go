package audiocheck

import (
	"math"
	"math/cmplx"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/hoganyugit/music163-mcp/internal/testaudio"
)

type fixture struct {
	name   string
	rate   int
	bits   int
	level  func(float64) float64
	padded bool // 以 16bit 量化后左移 8 位存成 24bit
}

func makeFixture(t testing.TB, dir string, fx fixture, seconds float64) string {
	t.Helper()
	path := filepath.Join(dir, fx.name+".flac")
	if err := testaudio.MakeFLAC(path, fx.rate, fx.bits, seconds, fx.level, fx.padded); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	musicLevel = testaudio.MusicLevel
	lowpass    = testaudio.Lowpass
	cdMaster   = testaudio.CDMaster
	hiresReal  = testaudio.HiresReal
)

func TestVerdicts(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		fx      fixture
		exp     Expect
		verdict Verdict
		check   func(t *testing.T, r *Report)
	}{
		{fixture{name: "cd", rate: 44100, bits: 16, level: cdMaster}, Expect{Format: "flac", Level: "lossless", DurationMs: 20000}, VerdictGenuine,
			func(t *testing.T, r *Report) {
				if r.Effective.Lossy || r.Effective.Rate != 44100 || r.Effective.Bits != 16 {
					t.Errorf("effective = %+v", r.Effective)
				}
			}},
		{fixture{name: "mp3-128", rate: 44100, bits: 16, level: lowpass(musicLevel, 16000, 300)}, Expect{}, VerdictLossyLikely,
			func(t *testing.T, r *Report) {
				if c := r.Spectrum.CliffHz; c < 15800 || c > 16600 {
					t.Errorf("cliff = %v, want ~16.1k", c)
				}
				if r.Confidence != "high" {
					t.Errorf("confidence = %s", r.Confidence)
				}
			}},
		{fixture{name: "mp3-192", rate: 44100, bits: 16, level: lowpass(musicLevel, 18600, 400)}, Expect{}, VerdictLossyLikely, nil},
		{fixture{name: "mp3-320", rate: 44100, bits: 16, level: lowpass(musicLevel, 20100, 500)}, Expect{}, VerdictLossySuspect, nil},
		{fixture{name: "upsampled", rate: 96000, bits: 24, level: cdMaster}, Expect{Level: "hires"}, VerdictUpsampled,
			func(t *testing.T, r *Report) {
				if r.Effective.Rate != 44100 || r.Effective.Bits != 0 {
					t.Errorf("effective = %+v", r.Effective)
				}
			}},
		{fixture{name: "upsampled-lossy", rate: 96000, bits: 24, level: lowpass(musicLevel, 16000, 300)}, Expect{}, VerdictLossyLikely,
			func(t *testing.T, r *Report) {
				if r.Effective.Rate != 44100 || !r.Effective.Lossy {
					t.Errorf("effective = %+v", r.Effective)
				}
			}},
		{fixture{name: "hires", rate: 96000, bits: 24, level: hiresReal}, Expect{Level: "hires"}, VerdictGenuine,
			func(t *testing.T, r *Report) {
				if r.Spectrum.UltrasonicDB == nil || *r.Spectrum.UltrasonicDB < 30 {
					t.Errorf("ultrasonic = %v", r.Spectrum.UltrasonicDB)
				}
			}},
		{fixture{name: "padded", rate: 96000, bits: 24, level: hiresReal, padded: true}, Expect{}, VerdictPadded,
			func(t *testing.T, r *Report) {
				if r.Bits.EffectiveBits != 16 || r.Effective.Bits != 16 || r.Effective.Rate != 96000 {
					t.Errorf("bits = %+v effective = %+v", r.Bits, r.Effective)
				}
			}},
		{fixture{name: "trial", rate: 44100, bits: 16, level: cdMaster}, Expect{DurationMs: 240000}, VerdictTrial, nil},
		{fixture{name: "mismatch", rate: 44100, bits: 16, level: cdMaster}, Expect{Level: "jymaster"}, VerdictMismatch, nil},
	}
	for _, tc := range cases {
		t.Run(tc.fx.name, func(t *testing.T) {
			path := makeFixture(t, dir, tc.fx, 20)
			png := filepath.Join(dir, tc.fx.name+".png")
			if keep := os.Getenv("AUDIOCHECK_KEEP"); keep != "" {
				png = filepath.Join(keep, tc.fx.name+".png")
			}
			r, err := Analyze(path, Options{Expect: tc.exp, Spectrogram: png})
			if err != nil {
				t.Fatal(err)
			}
			if r.Verdict != tc.verdict {
				t.Errorf("verdict = %s, want %s\nsummary: %s\nspectrum: %+v\nfindings: %+v", r.Verdict, tc.verdict, r.Summary, r.Spectrum, r.Findings)
			}
			if r.Spectrogram == "" {
				t.Errorf("spectrogram not written: %+v", r.Findings)
			}
			if tc.check != nil {
				tc.check(t, r)
			}
			t.Logf("%s: %s | %s | cliff %.0f Hz drop %.1f dB, bw %.0f Hz, %d ms", tc.fx.name, r.Verdict, r.EffectiveSpec,
				r.Spectrum.CliffHz, r.Spectrum.CliffDropDB, r.Spectrum.BandwidthHz, r.AnalysisMs)
		})
	}
}

func TestCorruptFLAC(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir, fixture{name: "corrupt", rate: 44100, bits: 16, level: cdMaster}, 10)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0x5a
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != VerdictCorrupt {
		t.Fatalf("verdict = %s, findings %+v", r.Verdict, r.Findings)
	}
}

func TestTruncatedFLAC(t *testing.T) {
	dir := t.TempDir()
	path := makeFixture(t, dir, fixture{name: "cut", rate: 44100, bits: 16, level: cdMaster}, 10)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, st.Size()*6/10); err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != VerdictCorrupt {
		t.Fatalf("verdict = %s, findings %+v", r.Verdict, r.Findings)
	}
}

func TestUnsupportedFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.m4a")
	if err := os.WriteFile(path, []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Analyze(path, Options{}); err == nil {
		t.Fatal("expected error for mp4")
	}
}

func TestFFTMatchesNaiveDFT(t *testing.T) {
	const n = 64
	rng := rand.New(rand.NewSource(3))
	x := make([]complex128, n)
	for i := range x {
		x[i] = complex(rng.NormFloat64(), rng.NormFloat64())
	}
	want := make([]complex128, n)
	for k := range want {
		for i, v := range x {
			want[k] += v * cmplx.Exp(complex(0, -2*math.Pi*float64(k*i)/n))
		}
	}
	got := append([]complex128(nil), x...)
	newFFTPlan(n).transform(got)
	for k := range got {
		if cmplx.Abs(got[k]-want[k]) > 1e-9 {
			t.Fatalf("bin %d: got %v want %v", k, got[k], want[k])
		}
	}
}

func TestQuantFloorMatchesDither(t *testing.T) {
	// 纯量化噪声（白噪声，方差 q²/12）的平均频谱应落在理论噪底附近
	const rate, bits = 44100, 16
	rng := rand.New(rand.NewSource(5))
	n := int(rate * 4)
	a := newAnalyzer(rate, 2, bits, int64(n))
	l, r := make([]int32, n), make([]int32, n)
	for i := range l {
		// 均匀分布在 ±0.5 LSB 内的噪声等效于量化误差；放大 64 倍避免被判为静音，再按 64² 换算回去
		l[i] = int32(math.Round((rng.Float64() - 0.5) * 64 * 4))
		r[i] = int32(math.Round((rng.Float64() - 0.5) * 64 * 4))
	}
	a.feed([][]int32{l, r})
	mp := a.meanPower()
	if mp == nil {
		t.Fatal("no windows")
	}
	var s float64
	for _, p := range mp[100 : len(mp)-100] {
		s += p
	}
	got := dbOf(s / float64(len(mp)-200))
	// 方差 = (256 LSB)²/12，对应 256² 倍量化噪声 = +48.2 dB
	want := quantFloorDB(bits, a.n) + 20*math.Log10(256)
	if math.Abs(got-want) > 1.0 {
		t.Fatalf("noise level %.2f dB, want %.2f dB", got, want)
	}
}

// AUDIOCHECK_BENCH=1 时测一段 60 秒 24bit/192kHz 文件的分析耗时
func TestSpeed192k(t *testing.T) {
	if os.Getenv("AUDIOCHECK_BENCH") == "" {
		t.Skip("set AUDIOCHECK_BENCH=1")
	}
	dir := t.TempDir()
	path := makeFixture(t, dir, fixture{name: "speed", rate: 192000, bits: 24, level: hiresReal}, 60)
	st, _ := os.Stat(path)
	r, err := Analyze(path, Options{Spectrogram: filepath.Join(dir, "speed.png")})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("60s 24/192 (%d MB): %s in %d ms", st.Size()>>20, r.Verdict, r.AnalysisMs)
}
