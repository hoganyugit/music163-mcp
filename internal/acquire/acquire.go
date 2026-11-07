// Package acquire 负责"下到真正最高音质"：逐档探测、下载、检测、比较信息量，
// 只有实测与标称一致时才保留最高档，否则换成信息量相同但体积更小的档位
package acquire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	marker "github.com/XiaoMengXinX/163KeyMarker"
	"github.com/sirupsen/logrus"

	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/config"
	"github.com/hoganyugit/music163-mcp/internal/fetch"
	"github.com/hoganyugit/music163-mcp/internal/netease"
	"github.com/hoganyugit/music163-mcp/internal/textx"
)

// API 是用到的网易云接口
type API interface {
	SongMeta(musicID int) (netease.SongMeta, error)
	SongURL(musicID int, level string) (netease.SongURL, error)
	Lyric(musicID int) (lrc, trans string, err error)
}

// Fetcher 把 URL 下载到下载目录下的 fileName
type Fetcher interface {
	DownloadMusic(ctx context.Context, url, fileName string, progress func(fetch.Progress)) error
	DownloadFile(url, fileName string) error
}

// ProgressFunc 报告阶段与字节进度，total 为 0 表示未知
type ProgressFunc func(stage string, done, total int64)

type Options struct {
	Level string // auto（默认）或指定档位
	Force bool   // 已下载过也重新下载
}

// Attempt 记录每个档位的处理结果
type Attempt struct {
	Requested   string             `json:"requested"`
	Level       string             `json:"level,omitempty"`
	LevelName   string             `json:"level_name,omitempty"`
	Status      string             `json:"status"` // kept / replaced / rejected / skipped / unavailable / trial / error
	Note        string             `json:"note,omitempty"`
	Verdict     audiocheck.Verdict `json:"verdict,omitempty"`
	Effective   string             `json:"effective,omitempty"`
	Summary     string             `json:"summary,omitempty"`
	SizeMB      float64            `json:"size_mb,omitempty"`
	Spectrogram string             `json:"spectrogram,omitempty"`
}

type Result struct {
	MusicID        int                `json:"music_id"`
	Title          string             `json:"title"`
	Artists        string             `json:"artists"`
	Album          string             `json:"album"`
	Level          string             `json:"level"`
	LevelName      string             `json:"level_name"`
	Path           string             `json:"path"`
	LyricsPath     string             `json:"lyrics_path,omitempty"`
	SizeMB         float64            `json:"size_mb"`
	Decision       string             `json:"decision"`
	Warnings       []string           `json:"warnings,omitempty"`
	Report         *audiocheck.Report `json:"report"`
	Attempts       []Attempt          `json:"attempts"`
	ReportPath     string             `json:"report_path"`
	FinishedAt     time.Time          `json:"finished_at"`
	AlreadyExisted bool               `json:"already_existed,omitempty"`
}

type Service struct {
	api      API
	dl       Fetcher
	lib      config.LibraryConfig
	incoming string
	log      *logrus.Logger
	sem      chan struct{}

	mu   sync.Mutex
	busy map[int]bool
}

// IncomingDir 是 Fetcher 的保存目录
func IncomingDir(lib config.LibraryConfig) string { return filepath.Join(lib.StateDir, "incoming") }

func New(api API, dl Fetcher, lib config.LibraryConfig, concurrency int, log *logrus.Logger) (*Service, error) {
	if log == nil {
		log = logrus.New()
		log.SetOutput(io.Discard)
	}
	s := &Service{
		api:      api,
		dl:       dl,
		lib:      lib,
		incoming: IncomingDir(lib),
		log:      log,
		sem:      make(chan struct{}, max(1, concurrency)),
		busy:     map[int]bool{},
	}
	for _, d := range []string{s.incoming, filepath.Join(lib.StateDir, "reports"), filepath.Join(lib.StateDir, "spectrograms")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	s.cleanIncoming(24 * time.Hour)
	return s, nil
}

// cleanIncoming 删除下载目录里异常退出遗留的旧文件
func (s *Service) cleanIncoming(age time.Duration) {
	entries, err := os.ReadDir(s.incoming)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) > age {
			_ = os.Remove(filepath.Join(s.incoming, e.Name()))
		}
	}
}

// 请求间隔由 netease.Client 的 Gate 统一控制（见 cmd/music163-mcp），这里不再单独限速
func (s *Service) songMeta(id int) (netease.SongMeta, error) { return s.api.SongMeta(id) }

func (s *Service) songURL(id int, level string) (netease.SongURL, error) {
	return s.api.SongURL(id, level)
}

func unavailableReason(u netease.SongURL) string {
	switch {
	case u.Code == 404:
		return "没有这一档的资源"
	case u.Fee == 4:
		return "需要购买数字专辑"
	case u.Fee == 1:
		return "需要会员"
	}
	return fmt.Sprintf("拿不到下载链接（code %d, fee %d）", u.Code, u.Fee)
}

func mb(n int64) float64 { return math.Round(float64(n)/1024/1024*10) / 10 }

// LevelProbe 是某个档位的实际可得情况
type LevelProbe struct {
	Requested     string  `json:"requested"`
	RequestedName string  `json:"requested_name"`
	Available     bool    `json:"available"`
	Level         string  `json:"level,omitempty"`
	LevelName     string  `json:"level_name,omitempty"`
	Format        string  `json:"format,omitempty"`
	Kbps          int     `json:"kbps,omitempty"`
	SizeMB        float64 `json:"size_mb,omitempty"`
	Trial         bool    `json:"trial,omitempty"`
	Note          string  `json:"note,omitempty"`
}

// Probe 逐档请求链接（不下载），报告每档实际给了什么
func (s *Service) Probe(ctx context.Context, musicID int, spatial bool) (netease.SongMeta, []LevelProbe, error) {
	meta, err := s.songMeta(musicID)
	if err != nil {
		return meta, nil, err
	}
	levels := append([]string(nil), netease.StereoLevels...)
	if spatial {
		levels = append(levels, netease.SpatialLevels...)
	}
	seen := map[string]string{}
	var out []LevelProbe
	for _, lv := range levels {
		if err := ctx.Err(); err != nil {
			return meta, out, err
		}
		p := LevelProbe{Requested: lv, RequestedName: netease.LevelName(lv)}
		u, err := s.songURL(musicID, lv)
		switch {
		case err != nil:
			p.Note = "查询失败: " + err.Error()
		case !u.Available():
			p.Note = unavailableReason(u)
		default:
			p.Available = true
			p.Level, p.LevelName = u.Level, netease.LevelName(u.Level)
			p.Format, p.Kbps, p.SizeMB, p.Trial = u.Type, u.Bitrate/1000, mb(u.Size), u.Trial
			var notes []string
			if u.Level != "" && u.Level != lv {
				notes = append(notes, "这一档没有资源或权限，服务器给的是"+netease.LevelName(u.Level))
			}
			if u.Trial {
				notes = append(notes, "只给了试听片段")
			}
			if prev, ok := seen[u.MD5]; ok && u.MD5 != "" {
				notes = append(notes, "与"+netease.LevelName(prev)+"是同一个文件")
			} else if u.MD5 != "" {
				seen[u.MD5] = lv
			}
			p.Note = strings.Join(notes, "；")
		}
		out = append(out, p)
	}
	return meta, out, nil
}

// LibraryStatus 是曲库所在存储的状态
type LibraryStatus struct {
	Dir     string  `json:"dir"`
	Mount   string  `json:"mount,omitempty"`
	Mounted bool    `json:"mounted"`
	FreeGB  float64 `json:"free_gb,omitempty"`
}

func (s *Service) LibraryStatus() LibraryStatus {
	st := LibraryStatus{Dir: s.lib.Dir, Mount: s.lib.Mount, Mounted: true}
	if s.lib.Mount != "" {
		st.Mounted = isMountPoint(s.lib.Mount)
	}
	if !st.Mounted {
		return st
	}
	// 曲库目录可能还没建，往上找到存在的目录再查剩余空间
	p := s.lib.Dir
	for {
		if _, err := os.Stat(p); err == nil || p == filepath.Dir(p) {
			break
		}
		p = filepath.Dir(p)
	}
	if free, err := freeBytes(p); err == nil {
		st.FreeGB = math.Round(float64(free)/1e9*10) / 10
	}
	return st
}

// checkLibrary 曲库在移动存储上而存储没挂载时拒绝写入，避免把歌写到电脑硬盘的空目录里
func (s *Service) checkLibrary() error {
	if s.lib.Mount != "" && !isMountPoint(s.lib.Mount) {
		return fmt.Errorf("曲库所在的存储没有挂载（%s）：SD 卡或读卡器没插上，插上后再下载。为避免写到电脑硬盘，这次没有下载", s.lib.Mount)
	}
	return nil
}

func (s *Service) lock(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return fmt.Errorf("歌曲 %d 正在下载中", id)
	}
	s.busy[id] = true
	return nil
}

func (s *Service) unlock(id int) {
	s.mu.Lock()
	delete(s.busy, id)
	s.mu.Unlock()
}

// Download 下载一首歌并入库；Level 为 auto 时自动选出"真正最高"的档位
func (s *Service) Download(ctx context.Context, musicID int, opt Options, progress ProgressFunc) (*Result, error) {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	level := opt.Level
	if level == "" {
		level = "auto"
	}
	if level != "auto" && !netease.ValidLevel(level) {
		return nil, fmt.Errorf("未知档位 %q", level)
	}
	if err := s.checkLibrary(); err != nil {
		return nil, err
	}
	if err := s.lock(musicID); err != nil {
		return nil, err
	}
	defer s.unlock(musicID)
	if !opt.Force {
		if r, ok := s.existing(musicID); ok {
			return r, nil
		}
	}

	progress("排队等待", 0, 0)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.sem }()

	progress("获取歌曲信息", 0, 0)
	meta, err := s.songMeta(musicID)
	if err != nil {
		return nil, fmt.Errorf("获取歌曲信息失败: %w", err)
	}
	if meta.Unavailable() {
		return nil, fmt.Errorf("网易云不让播放《%s》：可能是没有版权或已下架，也可能是地区限制。"+
			"如果网页版或 App 在国内能播，可在配置 [netease] proxy 填一个国内代理再试", meta.Name)
	}
	r := &run{s: s, ctx: ctx, meta: meta, progress: progress, tried: map[string]bool{}}
	defer r.cleanup()
	var best *tier
	if level == "auto" {
		best, err = r.auto()
	} else {
		best, err = r.single(level)
	}
	if err != nil {
		return nil, err
	}
	return r.finish(best)
}

type tier struct {
	level   string
	format  string
	url     netease.SongURL
	path    string
	size    int64
	report  *audiocheck.Report
	attempt int
}

type run struct {
	s        *Service
	ctx      context.Context
	meta     netease.SongMeta
	progress ProgressFunc
	attempts []Attempt
	temps    []string
	tried    map[string]bool
	warnings []string
}

func (r *run) cleanup() {
	for _, p := range r.temps {
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

func (r *run) note(requested, level, status, note string) {
	a := Attempt{Requested: requested, Level: level, Status: status, Note: note}
	if level != "" {
		a.LevelName = netease.LevelName(level)
	}
	r.attempts = append(r.attempts, a)
}

func (r *run) discard(t *tier) {
	_ = os.Remove(t.path)
}

// auto 从超清母带往下逐档下载检测：实测与标称一致即保留；否则继续往下比，
// 低一档的信息量不少于当前最佳就换成体积更小的低档，更少就停止
func (r *run) auto() (*tier, error) {
	var best *tier
	seenMD5 := map[string]string{}
	trial := false
	levels := netease.StereoLevels
loop:
	for _, lv := range levels {
		if r.tried[lv] {
			continue
		}
		if best != nil && !canStillWin(lv, best.report.Effective, best.format) {
			break
		}
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		r.progress("查询"+netease.LevelName(lv)+"链接", 0, 0)
		u, err := r.s.songURL(r.meta.ID, lv)
		if err != nil {
			r.note(lv, "", "error", "获取链接失败: "+err.Error())
			continue
		}
		if !u.Available() {
			r.note(lv, "", "unavailable", unavailableReason(u))
			continue
		}
		actual := u.Level
		if actual == "" {
			actual = lv
		}
		if slices.Contains(netease.SpatialLevels, actual) {
			// 没有这一档时服务器可能悄悄给空间音频处理版，那不是原始立体声混音，auto 不用
			r.tried[lv] = true
			r.note(lv, actual, "skipped", "服务器给的是"+netease.LevelName(actual)+"（空间音频处理版，不是原始立体声混音），不用")
			continue
		}
		if actual != lv && r.tried[actual] {
			r.note(lv, actual, "skipped", "服务器给的是"+netease.LevelName(actual)+"，已经处理过")
			continue
		}
		r.tried[lv], r.tried[actual] = true, true
		if u.Trial {
			trial = true
			r.note(lv, actual, "trial", "只给了试听片段，不下载")
			continue
		}
		if prev, ok := seenMD5[u.MD5]; ok && u.MD5 != "" {
			r.note(lv, actual, "skipped", "与"+netease.LevelName(prev)+"是同一个文件")
			continue
		}
		if best != nil && !canStillWin(actual, best.report.Effective, best.format) {
			break
		}
		t, err := r.fetch(u, actual)
		if err != nil {
			if r.ctx.Err() != nil {
				return nil, r.ctx.Err()
			}
			r.note(lv, actual, "error", err.Error())
			continue
		}
		if u.MD5 != "" {
			seenMD5[u.MD5] = actual
		}
		if actual != lv {
			r.attempts[t.attempt].Note = "请求" + netease.LevelName(lv) + "，服务器给的是" + netease.LevelName(actual)
		}
		v := t.report.Verdict
		if v == audiocheck.VerdictCorrupt || v == audiocheck.VerdictTrial {
			r.attempts[t.attempt].Status = "rejected"
			r.discard(t)
			if v == audiocheck.VerdictTrial {
				trial = true
			}
			continue
		}
		switch {
		case best == nil:
			best = t
		case compareEffective(t.report.Effective, best.report.Effective) >= 0:
			a := &r.attempts[best.attempt]
			a.Status = "replaced"
			a.Note = joinNote(a.Note, fmt.Sprintf("实际信息量与%s相当，换用体积更小的%s", netease.LevelName(t.level), netease.LevelName(t.level)))
			r.discard(best)
			best = t
		default:
			a := &r.attempts[t.attempt]
			a.Status = "rejected"
			a.Note = joinNote(a.Note, "实际信息量比"+netease.LevelName(best.level)+"少")
			r.discard(t)
			break loop
		}
		if v == audiocheck.VerdictGenuine {
			break loop
		}
	}
	if best == nil {
		if trial {
			return nil, errors.New("这首歌只能拿到试听片段（可能需要单独购买数字专辑），没有下载")
		}
		var reasons []string
		for _, a := range r.attempts {
			reasons = append(reasons, netease.LevelName(a.Requested)+": "+a.Note)
		}
		return nil, fmt.Errorf("没有拿到可用的完整文件（%s）", strings.Join(reasons, "；"))
	}
	return best, nil
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

// single 只下载指定档位，检测结果仅作报告
func (r *run) single(level string) (*tier, error) {
	u, err := r.s.songURL(r.meta.ID, level)
	if err != nil {
		return nil, fmt.Errorf("获取链接失败: %w", err)
	}
	if !u.Available() {
		return nil, fmt.Errorf("%s: %s", netease.LevelName(level), unavailableReason(u))
	}
	if u.Trial {
		return nil, fmt.Errorf("%s 只给了试听片段（可能需要单独购买数字专辑）", netease.LevelName(level))
	}
	actual := u.Level
	if actual == "" {
		actual = level
	}
	t, err := r.fetch(u, actual)
	if err != nil {
		return nil, err
	}
	if actual != level {
		r.attempts[t.attempt].Note = "请求" + netease.LevelName(level) + "，服务器给的是" + netease.LevelName(actual)
	}
	if v := t.report.Verdict; v == audiocheck.VerdictCorrupt || v == audiocheck.VerdictTrial {
		r.attempts[t.attempt].Status = "rejected"
		return nil, fmt.Errorf("%s 文件不可用: %s", netease.LevelName(actual), t.report.Summary)
	}
	return t, nil
}

func fileFormat(u netease.SongURL) string {
	switch u.Type {
	case "flac", "mp3":
		return u.Type
	}
	switch strings.ToLower(path.Ext(textx.URLBase(u.URL))) {
	case ".flac":
		return "flac"
	case ".mp3":
		return "mp3"
	}
	return u.Type
}

// fetch 下载一个档位、校验 MD5 并检测
func (r *run) fetch(u netease.SongURL, level string) (*tier, error) {
	format := fileFormat(u)
	if format != "flac" && format != "mp3" {
		return nil, fmt.Errorf("不支持的文件格式 %q", u.Type)
	}
	name := fmt.Sprintf("%d-%s-%d.%s", r.meta.ID, level, time.Now().UnixNano(), format)
	p := filepath.Join(r.s.incoming, name)
	r.temps = append(r.temps, p)
	stage := "下载" + netease.LevelName(level)
	download := func() error {
		r.progress(stage, 0, u.Size)
		return r.s.dl.DownloadMusic(r.ctx, u.URL, name, func(pr fetch.Progress) { r.progress(stage, pr.Written, pr.Total) })
	}
	if err := download(); err != nil {
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	if u.MD5 != "" {
		if err := fetch.VerifyMD5(p, u.MD5); err != nil {
			r.s.log.Warnf("歌曲 %d %s MD5 不符，重新下载", r.meta.ID, level)
			_ = os.Remove(p)
			if err := download(); err != nil {
				return nil, fmt.Errorf("下载失败: %w", err)
			}
			if err := fetch.VerifyMD5(p, u.MD5); err != nil {
				return nil, errors.New("两次下载的 MD5 都与接口不符，文件不完整")
			}
		}
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	r.progress("检测"+netease.LevelName(level), 0, 0)
	rep, err := audiocheck.Analyze(p, audiocheck.Options{
		Expect: audiocheck.Expect{
			Format:     format,
			Level:      level,
			DurationMs: r.meta.DurationMs,
			Trial:      u.Trial,
		},
		Spectrogram: r.s.spectrogramPath(r.meta.ID, level),
	})
	if err != nil {
		return nil, fmt.Errorf("检测失败: %w", err)
	}
	r.s.log.Infof("歌曲 %d %s: %s（%s）", r.meta.ID, level, rep.Verdict, rep.EffectiveSpec)
	t := &tier{level: level, format: format, url: u, path: p, size: st.Size(), report: rep, attempt: len(r.attempts)}
	r.attempts = append(r.attempts, Attempt{
		Requested:   u.Requested,
		Level:       level,
		LevelName:   netease.LevelName(level),
		Status:      "candidate",
		Verdict:     rep.Verdict,
		Effective:   rep.EffectiveSpec,
		Summary:     rep.Summary,
		SizeMB:      mb(st.Size()),
		Spectrogram: rep.Spectrogram,
	})
	return t, nil
}

// finish 写标签、封面、歌词，移入曲库并保存报告
func (r *run) finish(best *tier) (*Result, error) {
	s := r.s
	r.attempts[best.attempt].Status = "kept"
	r.progress("写入标签和封面", 0, 0)

	cover, mime := r.cover()
	rep := best.report
	t := tagSet{
		Title:   r.meta.Name,
		Artists: r.meta.ArtistNames(),
		Album:   r.meta.Album,
		Track:   r.meta.TrackNo,
		Disc:    discNumber(r.meta.Disc),
		Year:    r.meta.Year(),
		Key163:  create163Key(marker.CreateMarker(r.meta.Detail, best.url.Raw)),
		Quality: fmt.Sprintf("%s; %s; music163-mcp", rep.Verdict, rep.EffectiveSpec),
	}
	if len(t.Artists) > 0 {
		t.AlbumArtist = t.Artists[0]
	}
	switch rep.Verdict {
	case audiocheck.VerdictLossyLikely, audiocheck.VerdictLossySuspect:
		t.Comment = "⚠ " + rep.Summary
	case audiocheck.VerdictGenuine:
	default:
		t.Comment = rep.Summary
	}
	if err := writeTags(best.path, best.format, t, cover, mime); err != nil {
		r.warnings = append(r.warnings, "写标签失败: "+err.Error())
	}

	if err := s.checkLibrary(); err != nil {
		return nil, err
	}
	dest := s.claimPath(r.meta.ID, libraryPath(s.lib.Dir, r.meta, best.format))
	prev, _ := s.LoadResult(r.meta.ID)
	if err := moveFile(best.path, dest); err != nil {
		return nil, fmt.Errorf("移入曲库失败: %w", err)
	}
	if prev != nil && prev.Path != "" && prev.Path != dest {
		// 同一首歌换了档位/扩展名：删掉旧版本及其歌词
		_ = os.Remove(prev.Path)
		if prev.LyricsPath != "" && prev.LyricsPath != lyricsPathFor(dest) {
			_ = os.Remove(prev.LyricsPath)
		}
	}
	st, _ := os.Stat(dest)

	res := &Result{
		MusicID:    r.meta.ID,
		Title:      r.meta.Name,
		Artists:    strings.Join(r.meta.ArtistNames(), ", "),
		Album:      r.meta.Album,
		Level:      best.level,
		LevelName:  netease.LevelName(best.level),
		Path:       dest,
		Report:     rep,
		Attempts:   r.attempts,
		ReportPath: s.reportPath(r.meta.ID),
		FinishedAt: time.Now(),
	}
	if st != nil {
		res.SizeMB = mb(st.Size())
	}
	if s.lib.Lyrics {
		if p, err := r.lyrics(dest); err != nil {
			r.warnings = append(r.warnings, "歌词保存失败: "+err.Error())
		} else {
			res.LyricsPath = p
		}
	}
	res.Decision = r.decision(best)
	res.Warnings = r.warnings
	if err := s.saveResult(res); err != nil {
		res.Warnings = append(res.Warnings, "报告保存失败: "+err.Error())
	}
	return res, nil
}

func lyricsPathFor(audio string) string {
	return strings.TrimSuffix(audio, filepath.Ext(audio)) + ".lrc"
}

func (r *run) lyrics(dest string) (string, error) {
	lrc, trans, err := r.s.api.Lyric(r.meta.ID)
	if err != nil {
		return "", err
	}
	if !hasLRCTimestamps(lrc) {
		return "", nil
	}
	if r.s.lib.LyricsTranslation && hasLRCTimestamps(trans) {
		lrc = mergeLRC(lrc, trans)
	}
	p := lyricsPathFor(dest)
	return p, writeFileAtomic(p, []byte(lrc), 0o644)
}

func (r *run) cover() ([]byte, string) {
	pic := r.meta.PicURL
	if pic == "" {
		return nil, ""
	}
	ext := path.Ext(textx.URLBase(pic))
	if ext == "" || len(ext) > 5 {
		ext = ".jpg"
	}
	name := fmt.Sprintf("%d-cover-%d%s", r.meta.ID, time.Now().UnixNano(), ext)
	p := filepath.Join(r.s.incoming, name)
	r.temps = append(r.temps, p)
	if err := r.s.dl.DownloadFile(pic, name); err != nil {
		r.warnings = append(r.warnings, "封面下载失败: "+err.Error())
		return nil, ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		r.warnings = append(r.warnings, "封面读取失败: "+err.Error())
		return nil, ""
	}
	out, mime, err := prepareCover(data, r.s.lib.CoverMaxPx)
	if err != nil {
		r.warnings = append(r.warnings, err.Error())
		return nil, ""
	}
	return out, mime
}

// decision 用一两句话说明为什么保留这一档
func (r *run) decision(best *tier) string {
	rep := best.report
	name := netease.LevelName(best.level)
	var replaced, rejected []string
	sameFile := false
	for _, a := range r.attempts {
		switch a.Status {
		case "replaced":
			replaced = append(replaced, fmt.Sprintf("%s（%s，%.0f MB）", a.LevelName, a.Effective, a.SizeMB))
		case "rejected":
			rejected = append(rejected, a.LevelName)
		case "skipped":
			if strings.Contains(a.Note, "同一个文件") {
				sameFile = true
			}
		}
	}
	var b strings.Builder
	switch rep.Verdict {
	case audiocheck.VerdictGenuine:
		if len(replaced) == 0 {
			fmt.Fprintf(&b, "保留%s：实测 %s，与标称一致，这就是能拿到的最高真音质。", name, rep.EffectiveSpec)
		} else {
			fmt.Fprintf(&b, "更高档实测没有多出真实信息：%s。改存%s（%s，%.0f MB），信息量相同、体积更小。",
				strings.Join(replaced, "、"), name, rep.EffectiveSpec, mb(best.size))
		}
	case audiocheck.VerdictLossyLikely:
		fmt.Fprintf(&b, "保留%s并标记 ⚠。%s。各档都没有更好的版本，网易云这首很可能没有真无损。", name, rep.Summary)
	case audiocheck.VerdictLossySuspect:
		fmt.Fprintf(&b, "保留%s并标记 ⚠。%s。", name, rep.Summary)
	default:
		fmt.Fprintf(&b, "保留%s（%s）。%s。", name, rep.EffectiveSpec, rep.Summary)
		if len(replaced) > 0 {
			fmt.Fprintf(&b, "更高档 %s 没有多出真实信息。", strings.Join(replaced, "、"))
		} else if len(rejected) > 0 {
			fmt.Fprintf(&b, "更低的%s信息量更少，所以没有换档。", strings.Join(rejected, "、"))
		}
	}
	if sameFile {
		b.WriteString("部分档位其实是同一个文件，已跳过。")
	}
	return b.String()
}
