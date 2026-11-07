// Package mcpserver 把下载与检测能力包装成 MCP 工具
package mcpserver

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/XiaoMengXinX/Music163Api-Go/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"

	"github.com/hoganyugit/music163-mcp/internal/acquire"
	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/config"
	"github.com/hoganyugit/music163-mcp/internal/netease"
	"github.com/hoganyugit/music163-mcp/internal/textx"
)

// Netease 是搜索与账号相关的接口
type Netease interface {
	SearchSong(keyword string, limit, offset int) (types.SearchSongData, error)
	SongMetas(ids []int) ([]netease.SongMeta, error)
	Account() (netease.Account, error)
	VipStatus() ([]netease.VipItem, error)
}

type Acquirer interface {
	Probe(ctx context.Context, musicID int, spatial bool) (netease.SongMeta, []acquire.LevelProbe, error)
	Download(ctx context.Context, musicID int, opt acquire.Options, progress acquire.ProgressFunc) (*acquire.Result, error)
	LibraryStatus() acquire.LibraryStatus
}

type Deps struct {
	Config  *config.Config
	Netease Netease
	Acquire Acquirer
	Log     *logrus.Logger
	Version string
	// ResolveShortLink 解析 163cn.tv 短链，测试时可替换
	ResolveShortLink func(string) string
}

type Server struct {
	d    Deps
	ctx  context.Context
	jobs *jobs
	srv  *mcp.Server
}

const instructions = `网易云音乐下载工具，目标是下到"真正"最高音质并识别假无损。
推荐流程：
1. search_songs 搜索，按歌名、歌手、专辑挑对版本（注意原版、Live、伴奏、翻唱的区别），拿到歌曲 ID；
2. download_song 下载。默认 level=auto：从超清母带往下逐档下载并检测，只有实测与标称一致才保留高档；
   升频的伪 Hi-Res 会自动换成信息量相同、体积更小的低档；疑似有损转码的文件会保留并标记 ⚠；
3. 下载可能要几分钟。没在等待时间内完成时会返回 job_id，用 job_status 继续等待，不要重复调用 download_song。
get_song_qualities 只查询各档位能拿到什么，不下载；check_audio 检测本地已有文件；account_status 查看登录和会员状态。
向用户汇报时请转述检测结论（verdict、effective_spec、decision），频谱检测是概率性的，"未发现造假痕迹"不等于百分之百真。`

func New(ctx context.Context, d Deps) *Server {
	if d.ResolveShortLink == nil {
		d.ResolveShortLink = textx.ResolveShortLink
	}
	if d.Log == nil {
		d.Log = logrus.New()
	}
	s := &Server{d: d, ctx: ctx, jobs: newJobs()}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "music163", Title: "网易云音乐（真无损检测）", Version: d.Version},
		&mcp.ServerOptions{Instructions: instructions})
	yes, no := true, false

	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "search_songs",
		Description: "搜索网易云歌曲，返回 ID、歌名、歌手、专辑、时长、别名和这首歌存在的最高档位。下载前先用它确认是哪个版本。",
		Annotations: &mcp.ToolAnnotations{Title: "搜索歌曲", ReadOnlyHint: true, OpenWorldHint: &yes},
	}, s.searchSongs)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "get_song_qualities",
		Description: "逐档查询当前账号实际能拿到的文件（不下载）：每档的格式、码率、大小、是否只给试听、是否被悄悄降档、哪些档其实是同一个文件。",
		Annotations: &mcp.ToolAnnotations{Title: "查询音质档位", ReadOnlyHint: true, OpenWorldHint: &yes},
	}, s.songQualities)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "download_song",
		Description: "下载一首歌到本地曲库（歌手/专辑/曲号 歌名.flac，含封面、标签、同名 .lrc 歌词），并检测真伪。" +
			"默认 level=auto 会逐档下载检测，只保留实测为真的最高档。返回保留的档位、检测结论、选择理由和频谱图；" +
			"超过等待时间会返回 job_id，用 job_status 继续等待。",
		Annotations: &mcp.ToolAnnotations{Title: "下载歌曲", DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes},
	}, s.downloadSong)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "job_status",
		Description: "查看或等待 download_song 返回的后台下载任务。",
		Annotations: &mcp.ToolAnnotations{Title: "下载任务状态", ReadOnlyHint: true, OpenWorldHint: &no},
	}, s.jobStatus)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name: "check_audio",
		Description: "检测本地 FLAC/MP3 文件是否名副其实：完整性（MD5）、试听片段、有损转码（假无损）、升频（伪 Hi-Res）、位深补零，" +
			"返回判定、证据和频谱图。只能读曲库和配置允许的目录。",
		Annotations: &mcp.ToolAnnotations{Title: "检测音频真伪", ReadOnlyHint: true, OpenWorldHint: &no},
	}, s.checkAudio)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "account_status",
		Description: "查看网易云登录是否有效、昵称和会员状态（不会返回登录凭据）。",
		Annotations: &mcp.ToolAnnotations{Title: "账号状态", ReadOnlyHint: true, OpenWorldHint: &yes},
	}, s.accountStatus)
	return s
}

func (s *Server) MCP() *mcp.Server { return s.srv }

// Drain 等后台下载任务结束（ctx 取消后它们会停下并删除临时文件），最多等 d，返回是否都已结束
func (s *Server) Drain(d time.Duration) bool { return s.jobs.drain(d) }

// parseSong 接受歌曲 ID、网易云歌曲链接或 163cn 短链
func (s *Server) parseSong(in string) (int, error) {
	in = strings.TrimSpace(in)
	if id, err := strconv.Atoi(in); err == nil && id > 0 {
		return id, nil
	}
	if textx.ContainsShortLink(in) {
		in = s.d.ResolveShortLink(in)
	}
	if textx.ContainsMusicLink(in) {
		if id := textx.ParseMusicID(in); id > 0 {
			return id, nil
		}
	}
	return 0, fmt.Errorf("无法识别歌曲 %q：请给歌曲 ID 或网易云歌曲链接；按名字找歌请先用 search_songs", in)
}

func textResult(text string, extra ...mcp.Content) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: append([]mcp.Content{&mcp.TextContent{Text: text}}, extra...)}
}

// SongBrief 是搜索结果中的一首歌
type SongBrief struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	Artists      string   `json:"artists"`
	Album        string   `json:"album"`
	Duration     string   `json:"duration"`
	Alias        []string `json:"alias,omitempty"`
	MaxLevel     string   `json:"max_level,omitempty"`
	MaxLevelName string   `json:"max_level_name,omitempty"`
	// StereoMax 是原始立体声混音里能确认的最高档（MaxLevel 可能是菁彩声/杜比等空间音频）
	StereoMax     string   `json:"stereo_max,omitempty"`
	StereoMaxName string   `json:"stereo_max_name,omitempty"`
	Unavailable   bool     `json:"unavailable,omitempty"` // 网易云没有版权或已下架
	Tags          []string `json:"tags,omitempty"`        // 如"含AI生成"
	Fee           int      `json:"fee"`
}

type searchIn struct {
	Keyword string `json:"keyword" jsonschema:"搜索关键词，如 歌名 歌手"`
	Limit   int    `json:"limit,omitempty" jsonschema:"返回条数，默认 10，最多 30"`
}

type searchOut struct {
	Results []SongBrief `json:"results"`
}

func briefFromMeta(m netease.SongMeta) SongBrief {
	b := SongBrief{
		ID: m.ID, Name: m.Name, Artists: strings.Join(m.ArtistNames(), " / "), Album: m.Album,
		Duration: fmtDuration(m.DurationMs), Alias: append(m.Alias, m.Trans...), Fee: m.Fee,
	}
	b.applyMeta(m)
	return b
}

func (b *SongBrief) applyMeta(m netease.SongMeta) {
	b.MaxLevel, b.MaxLevelName = m.MaxLevel, netease.LevelName(m.MaxLevel)
	b.StereoMax, b.StereoMaxName = m.StereoMax(), netease.LevelName(m.StereoMax())
	b.Unavailable, b.Tags = m.Unavailable(), m.Tags
	if b.Album == "" {
		b.Album = m.Album
	}
}

func (s *Server) searchSongs(ctx context.Context, req *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	var out searchOut
	kw := strings.TrimSpace(in.Keyword)
	if kw == "" {
		return nil, out, errors.New("keyword 不能为空")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 30)
	res, err := s.d.Netease.SearchSong(kw, limit, 0)
	if err != nil {
		return nil, out, fmt.Errorf("搜索失败: %w", err)
	}
	if res.Code != 200 {
		// 被限流时接口也会返回非 200，不能当成"没有搜到"
		return nil, out, fmt.Errorf("搜索失败：网易云返回 code %d%s。可能是请求太频繁被限流，请过几分钟再试，不要连续重试", res.Code, apiMessage(res.RawJson))
	}
	var ids []int
	for _, song := range res.Result.Songs {
		var artists []string
		for _, a := range song.Artists {
			artists = append(artists, a.Name)
		}
		var alias []string
		for _, a := range song.Alias {
			if str, ok := a.(string); ok && str != "" {
				alias = append(alias, str)
			}
		}
		out.Results = append(out.Results, SongBrief{
			ID: song.Id, Name: song.Name, Artists: strings.Join(artists, " / "), Album: song.Album.Name,
			Duration: fmtDuration(song.Duration), Alias: alias, Fee: song.Fee,
		})
		ids = append(ids, song.Id)
	}
	// 搜索结果不带档位信息，再批量查一次详情
	if metas, err := s.d.Netease.SongMetas(ids); err == nil {
		byID := map[int]netease.SongMeta{}
		for _, m := range metas {
			byID[m.ID] = m
		}
		for i := range out.Results {
			if m, ok := byID[out.Results[i].ID]; ok {
				out.Results[i].applyMeta(m)
			}
		}
	} else {
		s.d.Log.Warnf("查询搜索结果的档位失败: %v", err)
	}
	return textResult(formatSearch(kw, out.Results)), out, nil
}

// apiMessage 取出接口错误返回里的提示文字，格式为"（提示）"，没有时为空
func apiMessage(raw string) string {
	var v struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	if m := strings.TrimSpace(cmp.Or(v.Message, v.Msg)); m != "" {
		return "（" + m + "）"
	}
	return ""
}

type songIn struct {
	Song           string `json:"song" jsonschema:"歌曲 ID 或网易云歌曲链接（支持 163cn.tv 短链）"`
	IncludeSpatial bool   `json:"include_spatial,omitempty" jsonschema:"同时查询杜比全景声、沉浸环绕声、高清环绕声等空间音频版本，默认不查"`
}

type qualitiesOut struct {
	Song   SongBrief            `json:"song"`
	Levels []acquire.LevelProbe `json:"levels"`
}

func (s *Server) songQualities(ctx context.Context, req *mcp.CallToolRequest, in songIn) (*mcp.CallToolResult, qualitiesOut, error) {
	var out qualitiesOut
	id, err := s.parseSong(in.Song)
	if err != nil {
		return nil, out, err
	}
	meta, levels, err := s.d.Acquire.Probe(ctx, id, in.IncludeSpatial)
	if err != nil {
		return nil, out, err
	}
	out.Song, out.Levels = briefFromMeta(meta), levels
	return textResult(formatQualities(out.Song, levels)), out, nil
}

type downloadIn struct {
	Song        string `json:"song" jsonschema:"歌曲 ID 或网易云歌曲链接（支持 163cn.tv 短链）"`
	Level       string `json:"level,omitempty" jsonschema:"auto（默认）只保留实测为真的最高档；也可指定 jymaster、hires、lossless、exhigh 等只下这一档"`
	Force       bool   `json:"force,omitempty" jsonschema:"曲库里已有这首歌时也重新下载"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"最多等待秒数，默认 300，最大 1800；超时返回 job_id，下载在后台继续"`
}

type jobOut struct {
	JobID    string          `json:"job_id"`
	Status   string          `json:"status"` // running / done / failed
	Stage    string          `json:"stage,omitempty"`
	Progress string          `json:"progress,omitempty"`
	Error    string          `json:"error,omitempty"`
	Result   *acquire.Result `json:"result,omitempty"`
}

func waitDuration(v *int, def int) time.Duration {
	n := def
	if v != nil {
		n = *v
	}
	return time.Duration(max(0, min(n, 1800))) * time.Second
}

func (s *Server) downloadSong(ctx context.Context, req *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, jobOut, error) {
	id, err := s.parseSong(in.Song)
	if err != nil {
		return nil, jobOut{}, err
	}
	level := strings.ToLower(strings.TrimSpace(in.Level))
	if level == "" {
		level = "auto"
	}
	if level != "auto" && !netease.ValidLevel(level) {
		return nil, jobOut{}, fmt.Errorf("未知档位 %q，可选 auto/%s", in.Level, strings.Join(append(append([]string{}, netease.StereoLevels...), netease.SpatialLevels...), "/"))
	}
	opt := acquire.Options{Level: level, Force: in.Force}
	j, _ := s.jobs.start(s.ctx, id, level, func(ctx context.Context, progress acquire.ProgressFunc) (*acquire.Result, error) {
		return s.d.Acquire.Download(ctx, id, opt, progress)
	})
	return s.follow(ctx, req, j, waitDuration(in.WaitSeconds, 300))
}

type jobIn struct {
	JobID       string `json:"job_id" jsonschema:"download_song 返回的 job_id"`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"最多等待秒数，默认 120，最大 1800"`
}

func (s *Server) jobStatus(ctx context.Context, req *mcp.CallToolRequest, in jobIn) (*mcp.CallToolResult, jobOut, error) {
	j, ok := s.jobs.get(strings.TrimSpace(in.JobID))
	if !ok {
		return nil, jobOut{}, fmt.Errorf("找不到任务 %q（服务重启后任务记录会清空，可以重新调用 download_song，已下载的不会重复下载）", in.JobID)
	}
	return s.follow(ctx, req, j, waitDuration(in.WaitSeconds, 120))
}

// follow 等待任务，期间把进度转成 MCP 进度通知
func (s *Server) follow(ctx context.Context, req *mcp.CallToolRequest, j *job, wait time.Duration) (*mcp.CallToolResult, jobOut, error) {
	if token := req.Params.GetProgressToken(); token != nil && req.Session != nil {
		var last time.Time
		var lastStage string
		unsub := j.subscribe(func(ev progressEvent) {
			if ctx.Err() != nil || (ev.stage == lastStage && time.Since(last) < time.Second) {
				return
			}
			last, lastStage = time.Now(), ev.stage
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: ev.seq, Message: progressText(ev.stage, ev.done, ev.total)})
		})
		defer unsub()
	}
	j.wait(ctx, wait)
	snap := j.snapshot()
	out := jobOut{JobID: j.id, Stage: snap.stage}
	switch {
	case !snap.finished:
		out.Status = "running"
		out.Progress = progressText(snap.stage, snap.done, snap.total)
		return textResult(fmt.Sprintf("还在下载：%s。下载在后台继续，用 job_status（job_id=%s）等待结果。", out.Progress, j.id)), out, nil
	case snap.err != nil:
		out.Status, out.Error = "failed", snap.err.Error()
		res := textResult("下载失败：" + snap.err.Error())
		res.IsError = true
		return res, out, nil
	default:
		out.Status, out.Result, out.Stage = "done", snap.result, ""
		return textResult(formatResult(snap.result), s.images(snap.result)...), out, nil
	}
}

// images 附上保留档位的频谱图；换过档时再附上被换掉的最高档作为对照
func (s *Server) images(r *acquire.Result) []mcp.Content {
	var paths []string
	if r.Report != nil && r.Report.Spectrogram != "" {
		paths = append(paths, r.Report.Spectrogram)
	}
	for _, a := range r.Attempts {
		if a.Status == "replaced" && a.Spectrogram != "" {
			paths = append(paths, a.Spectrogram)
			break
		}
	}
	var out []mcp.Content
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			out = append(out, &mcp.ImageContent{Data: data, MIMEType: "image/png"})
		}
	}
	return out
}

type checkIn struct {
	Path                string  `json:"path" jsonschema:"本地 FLAC/MP3 文件的绝对路径（可用 ~ 表示家目录），必须在曲库或配置允许的目录里"`
	ExpectedDurationSec float64 `json:"expected_duration_sec,omitempty" jsonschema:"官方时长（秒），用于识别试听片段，可不填"`
}

func (s *Server) checkAudio(ctx context.Context, req *mcp.CallToolRequest, in checkIn) (*mcp.CallToolResult, *audiocheck.Report, error) {
	p, err := s.allowedPath(in.Path)
	if err != nil {
		return nil, nil, err
	}
	sum := sha1.Sum([]byte(p))
	png := filepath.Join(s.d.Config.Library.StateDir, "spectrograms", "check-"+hex.EncodeToString(sum[:8])+".png")
	rep, err := audiocheck.Analyze(p, audiocheck.Options{
		Expect:      audiocheck.Expect{DurationMs: int(in.ExpectedDurationSec * 1000)},
		Spectrogram: png,
	})
	if err != nil {
		return nil, nil, err
	}
	var imgs []mcp.Content
	if data, err := os.ReadFile(rep.Spectrogram); err == nil {
		imgs = append(imgs, &mcp.ImageContent{Data: data, MIMEType: "image/png"})
	}
	return textResult(FormatReport(rep), imgs...), rep, nil
}

// allowedPath 只允许读曲库和 check_dirs 里的普通文件（解析符号链接后判断）
func (s *Server) allowedPath(in string) (string, error) {
	p := strings.TrimSpace(in)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("请给绝对路径: %q", in)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("文件不存在或无法访问: %s", p)
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("不是普通文件: %s", p)
	}
	roots := s.d.Config.ReadDirs()
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(root)
		if err != nil {
			r = filepath.Clean(root)
		}
		if rel, err := filepath.Rel(r, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("出于安全考虑，只能检测这些目录里的文件：%s（可在配置 [library] check_dirs 里添加）", strings.Join(roots, "、"))
}

type vipOut struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Expire string `json:"expire,omitempty"`
}

type accountOut struct {
	LoggedIn bool                  `json:"logged_in"`
	Nickname string                `json:"nickname,omitempty"`
	UserID   int                   `json:"user_id,omitempty"`
	Vip      []vipOut              `json:"vip"`
	Library  acquire.LibraryStatus `json:"library"`
	Note     string                `json:"note,omitempty"`
}

func (s *Server) accountStatus(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, accountOut, error) {
	out := accountOut{Library: s.d.Acquire.LibraryStatus()}
	acc, err := s.d.Netease.Account()
	if err != nil {
		return nil, out, fmt.Errorf("查询登录状态失败: %w", err)
	}
	out.LoggedIn, out.Nickname, out.UserID = acc.LoggedIn, acc.Nickname, acc.UserID
	if !acc.LoggedIn {
		out.Note = "没有登录或登录已失效：请让用户在自己的终端运行 music163-mcp login 扫码（登录凭据不要经过对话），登录后立即生效，不用重启"
		return textResult(out.Note + "\n" + formatLibrary(out.Library)), out, nil
	}
	if items, err := s.d.Netease.VipStatus(); err == nil {
		for _, it := range items {
			v := vipOut{Name: it.Name, Active: it.Active}
			if !it.Expire.IsZero() {
				v.Expire = it.Expire.Format("2006-01-02")
			}
			out.Vip = append(out.Vip, v)
		}
	} else {
		out.Note = "会员信息查询失败: " + err.Error()
	}
	return textResult(formatAccount(out)), out, nil
}
