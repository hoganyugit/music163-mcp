package acquire

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/XiaoMengXinX/Music163Api-Go/types"
	"github.com/go-flac/flacpicture"
	"github.com/go-flac/flacvorbis"
	goflac "github.com/go-flac/go-flac"

	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/config"
	"github.com/hoganyugit/music163-mcp/internal/fetch"
	"github.com/hoganyugit/music163-mcp/internal/netease"
	"github.com/hoganyugit/music163-mcp/internal/testaudio"
)

// 各测试共用的合成音频，只生成一次
var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixtureErr  error
	fixtures    = map[string]string{}
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	fixtureOnce.Do(func() {
		fixtureDir, fixtureErr = os.MkdirTemp("", "acquire-fixtures-")
		if fixtureErr != nil {
			return
		}
		specs := []struct {
			name       string
			rate, bits int
			level      func(float64) float64
			padded     bool
		}{
			{"cd", 44100, 16, testaudio.CDMaster, false},
			{"up96", 96000, 24, testaudio.CDMaster, false},
			{"hires96", 96000, 24, testaudio.HiresReal, false},
			{"lossy44", 44100, 16, testaudio.Lowpass(testaudio.MusicLevel, 16000, 300), false},
			{"uplossy96", 96000, 24, testaudio.Lowpass(testaudio.MusicLevel, 16000, 300), false},
			{"padded96", 96000, 24, testaudio.HiresReal, true},
		}
		for _, s := range specs {
			p := filepath.Join(fixtureDir, s.name+".flac")
			if fixtureErr = testaudio.MakeFLAC(p, s.rate, s.bits, 12, s.level, s.padded); fixtureErr != nil {
				return
			}
			fixtures[s.name] = p
		}
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	p, ok := fixtures[name]
	if !ok {
		t.Fatalf("no fixture %q", name)
	}
	return p
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func fileMD5(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fakeAPI 按档位返回预设的链接；未配置的档位返回 404
type fakeAPI struct {
	meta  netease.SongMeta
	urls  map[string]netease.SongURL
	lyric string
}

func (f *fakeAPI) SongMeta(id int) (netease.SongMeta, error) { return f.meta, nil }
func (f *fakeAPI) SongURL(id int, level string) (netease.SongURL, error) {
	if u, ok := f.urls[level]; ok {
		u.Requested = level
		return u, nil
	}
	return netease.SongURL{Requested: level, Code: 404}, nil
}
func (f *fakeAPI) Lyric(id int) (string, string, error) {
	return f.lyric, "[00:01.00]译文第一句", nil
}

// fakeFetcher 把 URL 映射到本地文件，复制到下载目录
type fakeFetcher struct {
	dir       string
	files     map[string]string
	mu        sync.Mutex
	downloads []string
}

func (f *fakeFetcher) DownloadMusic(ctx context.Context, url, name string, progress func(fetch.Progress)) error {
	f.mu.Lock()
	f.downloads = append(f.downloads, url)
	f.mu.Unlock()
	src, ok := f.files[url]
	if !ok {
		return fmt.Errorf("404 %s", url)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	progress(fetch.Progress{Attempt: 1, Written: int64(len(data)), Total: int64(len(data))})
	return os.WriteFile(filepath.Join(f.dir, name), data, 0o644)
}

func (f *fakeFetcher) DownloadFile(url, name string) error {
	img := image.NewRGBA(image.Rect(0, 0, 1600, 1600))
	for y := 0; y < 1600; y += 7 {
		for x := 0; x < 1600; x += 3 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.dir, name), buf.Bytes(), 0o644)
}

type env struct {
	svc *Service
	api *fakeAPI
	dl  *fakeFetcher
	lib config.LibraryConfig
}

func newEnv(t *testing.T, tiers map[string]string, levelOverride map[string]string) *env {
	t.Helper()
	root := t.TempDir()
	lib := config.LibraryConfig{
		Dir:        filepath.Join(root, "Music", "NetEase"),
		StateDir:   filepath.Join(root, "state"),
		Lyrics:     true,
		CoverMaxPx: 1200,
	}
	dl := &fakeFetcher{dir: IncomingDir(lib), files: map[string]string{}}
	detail := types.SongDetailData{Name: "晴天", Id: 186016}
	detail.Al.Name = "叶惠美"
	api := &fakeAPI{
		meta: netease.SongMeta{
			ID: 186016, Name: "晴天", Artists: []netease.Artist{{ID: 6452, Name: "周杰伦"}},
			Album: "叶惠美", DurationMs: 12000, TrackNo: 3, Disc: "01", PublishTime: 1059580800000,
			PicURL: "http://p1.music.126.net/cover.jpg", Detail: detail,
		},
		urls:  map[string]netease.SongURL{},
		lyric: "[ti:晴天]\n[00:01.00]故事的小黄花\n[00:05.50]从出生那年就飘着",
	}
	for level, fx := range tiers {
		p := fixture(t, fx)
		url := "https://m701.music.126.net/" + level + "/" + fx + ".flac"
		dl.files[url] = p
		actual := level
		if o, ok := levelOverride[level]; ok {
			actual = o
		}
		st, _ := os.Stat(p)
		api.urls[level] = netease.SongURL{URL: url, Level: actual, Type: "flac", Size: st.Size(), MD5: fileMD5(t, p), Code: 200,
			Raw: types.SongURLData{Id: 186016, Url: url, Type: "flac", Md5: fileMD5(t, p)}}
	}
	svc, err := New(api, dl, lib, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &env{svc: svc, api: api, dl: dl, lib: lib}
}

func attemptStatus(r *Result) map[string]string {
	m := map[string]string{}
	for _, a := range r.Attempts {
		m[a.Requested] = a.Status
	}
	return m
}

// 请求超清母带时服务器悄悄给了高清环绕声（空间音频处理版），auto 不能把它当成最佳结果
func TestAutoSkipsSpatialSubstitution(t *testing.T) {
	e := newEnv(t,
		map[string]string{netease.LevelJymaster: "hires96", netease.LevelLossless: "cd"},
		map[string]string{netease.LevelJymaster: netease.LevelJyeffect})
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelLossless {
		t.Fatalf("kept %s (%s); attempts %+v", r.Level, r.LevelName, r.Attempts)
	}
	st := attemptStatus(r)
	if st[netease.LevelJymaster] != "skipped" || st[netease.LevelLossless] != "kept" {
		t.Fatalf("attempts %+v", r.Attempts)
	}
	for _, a := range r.Attempts {
		if a.Requested == netease.LevelJymaster && !strings.Contains(a.Note, "空间音频") {
			t.Errorf("jymaster note %q", a.Note)
		}
	}
	for _, d := range e.dl.downloads {
		if strings.Contains(d, "/"+netease.LevelJymaster+"/") {
			t.Errorf("spatial file was downloaded: %s", d)
		}
	}
}

func TestUpsampledMasterFallsBackToLossless(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "up96", netease.LevelLossless: "cd"}, nil)
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelLossless || r.Report.Verdict != audiocheck.VerdictGenuine {
		t.Fatalf("level %s verdict %s; attempts %+v", r.Level, r.Report.Verdict, r.Attempts)
	}
	st := attemptStatus(r)
	if st[netease.LevelJymaster] != "replaced" || st[netease.LevelHires] != "unavailable" || st[netease.LevelLossless] != "kept" {
		t.Fatalf("attempts %+v", r.Attempts)
	}
	if !strings.Contains(r.Decision, "改存无损") {
		t.Errorf("decision %q", r.Decision)
	}
	want := filepath.Join(e.lib.Dir, "周杰伦", "叶惠美", "03 晴天.flac")
	if r.Path != want {
		t.Fatalf("path %s, want %s", r.Path, want)
	}
	checkLibraryFile(t, r.Path, "")
	lrc, err := os.ReadFile(strings.TrimSuffix(r.Path, ".flac") + ".lrc")
	if err != nil || !strings.Contains(string(lrc), "故事的小黄花") {
		t.Fatalf("lrc %q %v", lrc, err)
	}
	if entries, _ := os.ReadDir(IncomingDir(e.lib)); len(entries) != 0 {
		t.Errorf("incoming not cleaned: %d files", len(entries))
	}
	if _, err := os.Stat(r.ReportPath); err != nil {
		t.Errorf("report not saved: %v", err)
	}

	// 再次请求直接返回已有结果，不重新下载
	n := len(e.dl.downloads)
	r2, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil || !r2.AlreadyExisted || len(e.dl.downloads) != n {
		t.Fatalf("second call: %+v %v downloads %d->%d", r2, err, n, len(e.dl.downloads))
	}
}

// checkLibraryFile 确认标签、封面写入且音频未被破坏
func checkLibraryFile(t *testing.T, p, wantComment string) {
	t.Helper()
	f, err := goflac.ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var vc *flacvorbis.MetaDataBlockVorbisComment
	var pic *flacpicture.MetadataBlockPicture
	for _, m := range f.Meta {
		switch m.Type {
		case goflac.VorbisComment:
			vc, err = flacvorbis.ParseFromMetaDataBlock(*m)
		case goflac.Picture:
			pic, err = flacpicture.ParseFromMetaDataBlock(*m)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if vc == nil || pic == nil {
		t.Fatal("missing vorbis comment or picture")
	}
	get := func(k string) string {
		v, _ := vc.Get(k)
		if len(v) == 0 {
			return ""
		}
		return v[0]
	}
	if get("TITLE") != "晴天" || get("ARTIST") != "周杰伦" || get("ALBUM") != "叶惠美" || get("ALBUMARTIST") != "周杰伦" ||
		get("TRACKNUMBER") != "3" || get("DISCNUMBER") != "1" || get("DATE") != "2003" || !strings.HasPrefix(get("DESCRIPTION"), "163 key") {
		t.Fatalf("tags: %v", vc.Comments)
	}
	if wantComment != "" && !strings.Contains(get("COMMENT"), wantComment) {
		t.Fatalf("COMMENT = %q, want %q", get("COMMENT"), wantComment)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(pic.ImageData))
	if err != nil || format != "jpeg" || cfg.Width != 1200 || pic.MIME != "image/jpeg" {
		t.Fatalf("cover %v %s %dx%d %s", err, format, cfg.Width, cfg.Height, pic.MIME)
	}
	rep, err := audiocheck.Analyze(p, audiocheck.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range rep.Findings {
		if fd.Check == "integrity" && fd.Status != "pass" {
			t.Fatalf("audio damaged after tagging: %+v", fd)
		}
	}
}

func TestGenuineMasterKeptWithoutCheckingLowerTiers(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "hires96", netease.LevelHires: "up96", netease.LevelLossless: "cd"}, nil)
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelJymaster || r.Report.Verdict != audiocheck.VerdictGenuine || len(e.dl.downloads) != 1 {
		t.Fatalf("level %s verdict %s downloads %v", r.Level, r.Report.Verdict, e.dl.downloads)
	}
	if !strings.Contains(r.Decision, "最高真音质") {
		t.Errorf("decision %q", r.Decision)
	}
}

func TestLossyEverywhereKeepsLosslessFlagged(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "uplossy96", netease.LevelLossless: "lossy44", netease.LevelExhigh: "lossy44"}, nil)
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelLossless || r.Report.Verdict != audiocheck.VerdictLossyLikely {
		t.Fatalf("level %s verdict %s attempts %+v", r.Level, r.Report.Verdict, r.Attempts)
	}
	for _, d := range e.dl.downloads {
		if strings.Contains(d, "/exhigh/") {
			t.Fatal("should not step down to MP3 tiers once a FLAC is in hand")
		}
	}
	if !strings.Contains(r.Decision, "⚠") {
		t.Errorf("decision %q", r.Decision)
	}
	checkLibraryFile(t, r.Path, "⚠")
}

func TestPaddedHiresBeatsLossless(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "padded96", netease.LevelLossless: "cd"}, nil)
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelJymaster || r.Report.Verdict != audiocheck.VerdictPadded {
		t.Fatalf("level %s verdict %s attempts %+v", r.Level, r.Report.Verdict, r.Attempts)
	}
	for _, d := range e.dl.downloads {
		if strings.Contains(d, "/lossless/") {
			t.Fatal("lossless cannot carry 96k content; should not be downloaded")
		}
	}
}

func TestSilentDowngradeDetected(t *testing.T) {
	// 没有 SVIP：请求超清母带和 Hi-Res，服务器都悄悄给了无损
	e := newEnv(t, map[string]string{netease.LevelJymaster: "cd", netease.LevelHires: "cd", netease.LevelLossless: "cd"},
		map[string]string{netease.LevelJymaster: netease.LevelLossless, netease.LevelHires: netease.LevelLossless})
	r, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelLossless || len(e.dl.downloads) != 1 {
		t.Fatalf("level %s downloads %v attempts %+v", r.Level, e.dl.downloads, r.Attempts)
	}
	if !strings.Contains(r.Attempts[0].Note, "服务器给的是无损") {
		t.Errorf("downgrade note missing: %+v", r.Attempts[0])
	}
}

func TestTrialOnlyFails(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelLossless: "cd"}, nil)
	u := e.api.urls[netease.LevelLossless]
	u.Trial = true
	e.api.urls[netease.LevelLossless] = u
	_, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "试听") {
		t.Fatalf("err = %v", err)
	}
	if len(e.dl.downloads) != 0 {
		t.Fatal("trial clip should not be downloaded")
	}
}

func TestExplicitLevelAndDuplicateSkip(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "up96", netease.LevelHires: "up96", netease.LevelLossless: "cd"}, nil)
	r, err := e.svc.Download(context.Background(), 186016, Options{Level: netease.LevelJymaster}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Level != netease.LevelJymaster || r.Report.Verdict != audiocheck.VerdictUpsampled || len(e.dl.downloads) != 1 {
		t.Fatalf("explicit level: %s %s %v", r.Level, r.Report.Verdict, e.dl.downloads)
	}

	// auto + force：hires 与超清母带是同一个文件，直接跳过，最后换成无损
	r, err = e.svc.Download(context.Background(), 186016, Options{Force: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := attemptStatus(r)
	if r.Level != netease.LevelLossless || st[netease.LevelHires] != "skipped" {
		t.Fatalf("level %s attempts %+v", r.Level, r.Attempts)
	}
	// 换档后旧的超清母带文件被删掉，曲库里只留一份
	if _, err := os.Stat(r.Path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(r.Path))
	var audio int
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".flac") {
			audio++
		}
	}
	if audio != 1 {
		t.Fatalf("library has %d audio files", audio)
	}
}

func TestProbe(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelJymaster: "up96", netease.LevelHires: "up96", netease.LevelLossless: "cd"},
		map[string]string{})
	_, probes, err := e.svc.Probe(context.Background(), 186016, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != len(netease.StereoLevels) {
		t.Fatalf("probes %d", len(probes))
	}
	if !strings.Contains(probes[1].Note, "同一个文件") || probes[3].Available {
		t.Fatalf("probes %+v", probes)
	}
	if len(e.dl.downloads) != 0 {
		t.Fatal("probe must not download")
	}
}

func TestPolicy(t *testing.T) {
	cd := audiocheck.Effective{Rate: 44100, Bits: 16}
	up := audiocheck.Effective{Rate: 44100, Bits: 0}
	up48 := audiocheck.Effective{Rate: 48000, Bits: 0}
	hr := audiocheck.Effective{Rate: 96000, Bits: 24}
	pad := audiocheck.Effective{Rate: 96000, Bits: 16}
	lossy := audiocheck.Effective{Lossy: true, CutoffHz: 16000, Rate: 44100, Bits: 16}
	lossy2 := audiocheck.Effective{Lossy: true, CutoffHz: 16300, Rate: 96000}
	cases := []struct {
		a, b audiocheck.Effective
		want int
	}{
		{cd, up, 0}, {up48, cd, 0}, {hr, cd, 1}, {pad, cd, 1}, {hr, pad, 1}, {cd, lossy, 1}, {lossy2, lossy, 0},
		{lossy, audiocheck.Effective{Lossy: true, CutoffHz: 19000}, -1},
	}
	for _, c := range cases {
		if got := compareEffective(c.a, c.b); got != c.want {
			t.Errorf("compare(%+v, %+v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	if canStillWin(netease.LevelLossless, hr, "flac") || !canStillWin(netease.LevelLossless, up, "flac") ||
		canStillWin(netease.LevelExhigh, lossy, "flac") || !canStillWin(netease.LevelHires, hr, "flac") {
		t.Error("canStillWin wrong")
	}
}

func TestSafeNameAndPath(t *testing.T) {
	if got := safeName(` AC/DC: Live? "x" `); got != `AC／DC： Live？ ＂x＂` {
		t.Errorf("safeName = %q", got)
	}
	if got := safeName("..."); got != "_" {
		t.Errorf("safeName dots = %q", got)
	}
	long := strings.Repeat("长", 200)
	if got := safeName(long); len(got) > maxNameBytes || !strings.HasPrefix(long, got) {
		t.Errorf("long name not truncated on rune boundary: %d bytes", len(got))
	}
	m := netease.SongMeta{Name: "Song", Artists: []netease.Artist{{Name: "A"}}, Album: "B", TrackNo: 7, Disc: "2/2"}
	if got := libraryPath("/lib", m, "flac"); got != "/lib/A/B/2-07 Song.flac" {
		t.Errorf("path = %s", got)
	}
	m = netease.SongMeta{Name: "Song"}
	if got := libraryPath("/lib", m, "mp3"); got != "/lib/未知歌手/未知专辑/Song.mp3" {
		t.Errorf("path = %s", got)
	}
}

func TestMergeLRCAndCover(t *testing.T) {
	got := mergeLRC("[ar:x]\n[00:01.00]a\n[00:02.5]b", "[00:01.000]甲\n[00:02.50]乙")
	want := "[ar:x]\n[00:01.00]a\n[00:01.000]甲\n[00:02.5]b\n[00:02.50]乙"
	if got != want {
		t.Errorf("mergeLRC =\n%s\nwant\n%s", got, want)
	}
	img := image.NewRGBA(image.Rect(0, 0, 600, 300))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	if !isBaselineJPEG(buf.Bytes()) {
		t.Error("Go JPEG should be baseline")
	}
	out, mime, err := prepareCover(buf.Bytes(), 1200)
	if err != nil || mime != "image/jpeg" || !bytes.Equal(out, buf.Bytes()) {
		t.Errorf("small baseline JPEG should be kept as-is: %v %s", err, mime)
	}
	progressive := append([]byte{0xFF, 0xD8, 0xFF, 0xC2, 0x00, 0x04, 0, 0}, buf.Bytes()[2:]...)
	if isBaselineJPEG(progressive) {
		t.Error("SOF2 should be detected as progressive")
	}
}

func TestUnmountedLibraryRefused(t *testing.T) {
	e := newEnv(t, map[string]string{netease.LevelLossless: "cd"}, nil)
	// 普通目录不是挂载点：模拟 SD 卡没插
	card := filepath.Join(t.TempDir(), "card")
	e.svc.lib.Mount = card
	e.svc.lib.Dir = filepath.Join(card, "Music", "NetEase")
	_, err := e.svc.Download(context.Background(), 186016, Options{}, nil)
	if err == nil || !strings.Contains(err.Error(), "没有挂载") {
		t.Fatalf("err = %v", err)
	}
	if len(e.dl.downloads) != 0 {
		t.Fatal("should not download when the card is missing")
	}
	if _, err := os.Stat(card); !os.IsNotExist(err) {
		t.Fatal("must not create the mount point directory")
	}
	if st := e.svc.LibraryStatus(); st.Mounted {
		t.Fatalf("status = %+v", st)
	}
	if !isMountPoint("/") || isMountPoint(t.TempDir()) {
		t.Fatal("isMountPoint wrong")
	}
	if _, err := os.Stat("/proc/self"); err == nil && !isMountPoint("/proc") {
		t.Fatal("/proc should be a mount point on Linux")
	}
}

// ACQUIRE_CROSSDEV_DIR 指向另一个文件系统（如 SD 卡）上的目录时，验证跨设备移动和中文/全角文件名
func TestMoveFileCrossDevice(t *testing.T) {
	dir := os.Getenv("ACQUIRE_CROSSDEV_DIR")
	if dir == "" {
		t.Skip("set ACQUIRE_CROSSDEV_DIR")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := filepath.Join(t.TempDir(), "x.flac")
	data := bytes.Repeat([]byte("music163"), 1<<17)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, safeName("周杰伦/测试"), safeName("叶惠美: Live?"), safeName("03 晴天 \"x\"")+".flac")
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source not removed")
	}
	if err := writeFileAtomic(strings.TrimSuffix(dst, ".flac")+".lrc", []byte("[00:01.00]歌词"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("ok: %s", dst)
}
