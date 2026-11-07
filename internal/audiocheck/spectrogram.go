package audiocheck

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	imgW, imgH = 1000, 470
	plotX      = 58
	plotY      = 30
	plotW      = 760
	plotH      = 400
	panelX     = 830
	panelW     = 160
	cmapBase   = 6 // 调色板前几项留给背景、文字和标注线
)

var (
	colBG    = color.RGBA{16, 16, 20, 255}
	colText  = color.RGBA{230, 230, 230, 255}
	colGuide = color.RGBA{150, 150, 160, 255}
	colCliff = color.RGBA{255, 70, 70, 255}
	colCurve = color.RGBA{90, 220, 255, 255}
	colAxis  = color.RGBA{80, 80, 90, 255}
)

// magma 色带的控制点
var magmaStops = []color.RGBA{
	{0, 0, 4, 255}, {28, 16, 68, 255}, {79, 18, 123, 255}, {129, 37, 129, 255},
	{181, 54, 122, 255}, {229, 80, 100, 255}, {251, 135, 97, 255},
	{254, 194, 135, 255}, {252, 253, 191, 255},
}

func magma(t float64) color.RGBA {
	t = math.Max(0, math.Min(1, t))
	pos := t * float64(len(magmaStops)-1)
	i := min(int(pos), len(magmaStops)-2)
	f := pos - float64(i)
	a, b := magmaStops[i], magmaStops[i+1]
	lerp := func(x, y uint8) uint8 { return uint8(math.Round(float64(x) + (float64(y)-float64(x))*f)) }
	return color.RGBA{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B), 255}
}

func dbOf(p float64) float64 { return 10 * math.Log10(p+1e-30) }

// renderSpectrogram 左侧为时间-频率图，右侧为整首歌的平均频谱曲线，并标出常见截止频率和检测到的断崖
func renderSpectrogram(path string, d *decoded, sp *Spectrum, rep *Report) error {
	cols := d.an.cols
	if len(cols) == 0 {
		return errors.New("没有可用的分析窗口")
	}
	pal := color.Palette{colBG, colText, colGuide, colCliff, colCurve, colAxis}
	ncm := 256 - cmapBase
	for i := 0; i < ncm; i++ {
		pal = append(pal, magma(float64(i)/float64(ncm-1)))
	}
	img := image.NewPaletted(image.Rect(0, 0, imgW, imgH), pal)

	// 色标范围：底端贴近量化噪底，顶端取 99.8 百分位
	var vals []float64
	for i, c := range cols {
		if d.an.silent[i] {
			continue
		}
		for _, v := range c {
			vals = append(vals, dbOf(float64(v)))
		}
	}
	if len(vals) == 0 {
		for _, c := range cols {
			for _, v := range c {
				vals = append(vals, dbOf(float64(v)))
			}
		}
	}
	slices.Sort(vals)
	floor := quantFloorDB(d.bits, d.an.n)
	hi := vals[len(vals)*998/1000] + 3
	lo := math.Max(floor-3, vals[len(vals)/100]-3)
	if hi-lo > 130 {
		lo = hi - 130
	}
	if hi-lo < 30 {
		lo = hi - 30
	}
	level := func(db float64) float64 { return math.Max(0, math.Min(1, (db-lo)/(hi-lo))) }

	for x := 0; x < plotW; x++ {
		c0 := x * len(cols) / plotW
		c1 := max((x+1)*len(cols)/plotW, c0+1)
		for y := 0; y < plotH; y++ {
			r := (plotH - 1 - y) * specRows / plotH
			var s float64
			for c := c0; c < c1; c++ {
				s += float64(cols[c][r])
			}
			t := level(dbOf(s / float64(c1-c0)))
			img.SetColorIndex(plotX+x, plotY+y, uint8(cmapBase+int(math.Round(t*float64(ncm-1)))))
		}
	}

	nyq := float64(d.rate) / 2
	yOf := func(f float64) int { return plotY + plotH - 1 - int(f/nyq*float64(plotH)) }
	hline := func(y, x0, x1 int, idx uint8, dashed bool) {
		for x := x0; x < x1; x++ {
			if !dashed || (x/4)%2 == 0 {
				img.SetColorIndex(x, y, idx)
			}
		}
	}

	// 参考线：常见有损截止频率与 CD/48k 的奈奎斯特频率
	guides := []struct {
		f     float64
		label string
	}{{16000, "16k"}, {19000, "19k"}, {20000, "20k"}, {22050, "22.05k"}, {24000, "24k"}, {48000, "48k"}}
	lastLabelY := math.MaxInt
	for _, g := range guides {
		if g.f >= nyq*0.995 {
			continue
		}
		y := yOf(g.f)
		hline(y, plotX, plotX+plotW, 2, true)
		hline(y, panelX, panelX+panelW, 5, true)
		if lastLabelY-y >= 12 {
			drawText(img, 4, y+4, g.label, colGuide)
			lastLabelY = y
		}
	}
	drawText(img, 4, plotY+plotH+4, "0", colText)
	drawText(img, 4, plotY+10, fmt.Sprintf("%gk", math.Round(nyq/100)/10), colText)

	if sp != nil && (sp.strongCliff() || sp.weakCliff()) {
		y := yOf(sp.CliffHz)
		hline(y, plotX, plotX+plotW, 3, false)
		hline(y, panelX, panelX+panelW, 3, false)
		drawText(img, panelX, y-4, fmt.Sprintf("cutoff %.1fk -%.0fdB", sp.CliffHz/1000, sp.CliffDropDB), colCliff)
	}

	// 右侧：平均频谱
	for y := plotY; y < plotY+plotH; y++ {
		img.SetColorIndex(panelX-1, y, 5)
		img.SetColorIndex(panelX+panelW, y, 5)
	}
	if sp != nil && len(sp.levels) > 0 {
		prev := -1
		for y := 0; y < plotH; y++ {
			f := float64(plotH-1-y) / float64(plotH) * nyq
			b := min(int(f/bandHz), len(sp.levels)-1)
			x := panelX + int(level(sp.levels[b])*float64(panelW-1))
			if prev >= 0 {
				for xx := min(prev, x); xx <= max(prev, x); xx++ {
					img.SetColorIndex(xx, plotY+y, 4)
				}
			}
			img.SetColorIndex(x, plotY+y, 4)
			prev = x
		}
	}
	drawText(img, panelX, plotY+plotH+16, fmt.Sprintf("%.0f", lo), colText)
	drawText(img, panelX+panelW-56, plotY+plotH+16, fmt.Sprintf("%.0f dB", hi), colText)
	drawText(img, panelX, plotY-8, "mean spectrum", colText)

	dur := float64(d.samples) / float64(d.rate)
	drawText(img, plotX, plotY+plotH+16, "0:00", colText)
	drawText(img, plotX+plotW-28, plotY+plotH+16, fmtDur(dur), colText)
	title := fmt.Sprintf("%s  %d Hz  %d bit  %d ch  %s   verdict: %s",
		strings.ToUpper(rep.Format), d.rate, d.bits, d.channels, fmtDur(dur), rep.Verdict)
	drawText(img, plotX, 18, title, colText)
	drawText(img, plotX, imgH-10, "guides: common lossy cutoffs (16/19/20k) and 44.1k/48k Nyquist (22.05/24k)", colGuide)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, img); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func drawText(img draw.Image, x, y int, s string, c color.Color) {
	dr := &font.Drawer{Dst: img, Src: image.NewUniform(c), Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	dr.DrawString(s)
}
