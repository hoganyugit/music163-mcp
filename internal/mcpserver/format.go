package mcpserver

import (
	"fmt"
	"strings"

	"github.com/hoganyugit/music163-mcp/internal/acquire"
	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
)

func fmtDuration(ms int) string {
	s := (ms + 500) / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func progressText(stage string, done, total int64) string {
	if total > 0 {
		return fmt.Sprintf("%s %.1f/%.1f MB（%d%%）", stage, float64(done)/1048576, float64(total)/1048576, done*100/total)
	}
	return stage
}

func formatSearch(kw string, rs []SongBrief) string {
	if len(rs) == 0 {
		return fmt.Sprintf("没有搜到“%s”。", kw)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "“%s”的搜索结果（下载时用 id）：\n", kw)
	for i, r := range rs {
		fmt.Fprintf(&b, "%d. %s - %s《%s》%s", i+1, r.Name, r.Artists, r.Album, r.Duration)
		if len(r.Alias) > 0 {
			fmt.Fprintf(&b, "（%s）", strings.Join(r.Alias, "；"))
		}
		switch {
		case r.Unavailable:
			b.WriteString(" [无版权或地区限制，下载不了]")
		case r.StereoMax != "" && r.StereoMax != r.MaxLevel:
			fmt.Fprintf(&b, " [立体声最高：%s；另有%s空间音频版]", r.StereoMaxName, r.MaxLevelName)
		case r.MaxLevelName != "":
			fmt.Fprintf(&b, " [最高档：%s]", r.MaxLevelName)
		}
		for _, t := range r.Tags {
			fmt.Fprintf(&b, " [%s]", t)
		}
		fmt.Fprintf(&b, " id=%d\n", r.ID)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatQualities(song SongBrief, levels []acquire.LevelProbe) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s - %s《%s》%s 各档位实际可得情况：\n", song.Name, song.Artists, song.Album, song.Duration)
	if song.Unavailable {
		b.WriteString("注意：网易云不让播放这首歌（没有版权、已下架或地区限制），各档都拿不到。地区限制可以在配置 [netease] proxy 填国内代理解决。\n")
	}
	for _, l := range levels {
		if !l.Available {
			fmt.Fprintf(&b, "- %s：不可用（%s）\n", l.RequestedName, l.Note)
			continue
		}
		fmt.Fprintf(&b, "- %s：%s %d kbps，%.1f MB", l.RequestedName, strings.ToUpper(l.Format), l.Kbps, l.SizeMB)
		if l.Note != "" {
			fmt.Fprintf(&b, "（%s）", l.Note)
		}
		b.WriteByte('\n')
	}
	b.WriteString("注：标称档位不等于真实音质，要知道真假需要 download_song 下载后检测。")
	return b.String()
}

var statusName = map[string]string{
	"kept":        "保留",
	"replaced":    "已换掉",
	"rejected":    "未采用",
	"skipped":     "跳过",
	"unavailable": "不可用",
	"trial":       "只有试听",
	"error":       "出错",
	"candidate":   "候选",
}

var verdictName = map[audiocheck.Verdict]string{
	audiocheck.VerdictGenuine:      "未发现造假痕迹",
	audiocheck.VerdictUpsampled:    "升频（伪 Hi-Res）",
	audiocheck.VerdictPadded:       "位深补零",
	audiocheck.VerdictLossySuspect: "可疑",
	audiocheck.VerdictLossyLikely:  "大概率有损转码",
	audiocheck.VerdictTrial:        "试听片段",
	audiocheck.VerdictCorrupt:      "损坏",
	audiocheck.VerdictMismatch:     "规格不符",
	audiocheck.VerdictInconclusive: "无法判断",
}

func formatResult(r *acquire.Result) string {
	var b strings.Builder
	head := "已下载"
	if r.AlreadyExisted {
		head = "曲库里已有（没有重新下载，需要的话用 force=true）"
	}
	fmt.Fprintf(&b, "%s：%s - %s《%s》\n", head, r.Title, r.Artists, r.Album)
	if r.Report != nil {
		fmt.Fprintf(&b, "保留档位：%s（实测 %s，%.1f MB）\n", r.LevelName, r.Report.EffectiveSpec, r.SizeMB)
		fmt.Fprintf(&b, "检测结论：%s（置信度 %s）\n", r.Report.Summary, r.Report.Confidence)
	}
	if r.Decision != "" {
		fmt.Fprintf(&b, "选择理由：%s\n", r.Decision)
	}
	fmt.Fprintf(&b, "文件：%s\n", r.Path)
	if r.LyricsPath != "" {
		fmt.Fprintf(&b, "歌词：%s\n", r.LyricsPath)
	}
	if len(r.Attempts) > 0 {
		b.WriteString("各档处理：\n")
		for _, a := range r.Attempts {
			name := a.LevelName
			if name == "" || a.Requested != a.Level {
				name = levelLabel(a)
			}
			fmt.Fprintf(&b, "- %s：%s", name, statusName[a.Status])
			if a.Verdict != "" {
				name := verdictName[a.Verdict]
				if a.Verdict == audiocheck.VerdictGenuine && strings.Contains(a.Effective, "MP3") {
					name = "与码率相符"
				}
				fmt.Fprintf(&b, "，%s（%s）", name, a.Effective)
			}
			if a.Note != "" {
				fmt.Fprintf(&b, "，%s", a.Note)
			}
			b.WriteByte('\n')
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "注意：%s\n", w)
	}
	if r.Report != nil && r.Report.Spectrogram != "" {
		fmt.Fprintf(&b, "频谱图：%s\n", r.Report.Spectrogram)
	}
	return strings.TrimRight(b.String(), "\n")
}

func levelLabel(a acquire.Attempt) string {
	req := a.Requested
	if n, ok := levelNames[req]; ok {
		req = n
	}
	if a.LevelName != "" && a.Level != a.Requested {
		return req + "→" + a.LevelName
	}
	return req
}

var levelNames = map[string]string{
	"standard": "标准", "higher": "较高", "exhigh": "极高", "lossless": "无损", "hires": "Hi-Res",
	"jyeffect": "高清环绕声", "sky": "沉浸环绕声", "dolby": "杜比全景声", "jymaster": "超清母带",
}

// FormatReport 把检测报告排版成给人看的中文文本
func FormatReport(r *audiocheck.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", r.File)
	fmt.Fprintf(&b, "格式：%s %dbit/%dHz %d 声道，时长 %s，平均 %d kbps\n", strings.ToUpper(r.Format), r.BitsPerSample, r.SampleRate, r.Channels,
		fmtDuration(int(r.DurationSec*1000)), r.BitrateKbps)
	name := verdictName[r.Verdict]
	if r.Format == "mp3" && r.Verdict == audiocheck.VerdictGenuine {
		name = "有损格式，与码率相符"
	}
	fmt.Fprintf(&b, "判定：%s（%s，置信度 %s）\n", name, r.Verdict, r.Confidence)
	fmt.Fprintf(&b, "结论：%s\n", r.Summary)
	fmt.Fprintf(&b, "实际规格：%s\n", r.EffectiveSpec)
	b.WriteString("各项检查：\n")
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "- [%s] %s：%s\n", f.Status, f.Check, f.Detail)
	}
	if r.Spectrogram != "" {
		fmt.Fprintf(&b, "频谱图：%s\n", r.Spectrogram)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatAccount(a accountOut) string {
	var b strings.Builder
	fmt.Fprintf(&b, "已登录：%s（uid %d）\n", a.Nickname, a.UserID)
	if len(a.Vip) == 0 {
		b.WriteString("会员：接口没有返回会员信息\n")
	}
	for _, v := range a.Vip {
		state := "已过期"
		if v.Active {
			state = "生效中"
		}
		fmt.Fprintf(&b, "- %s：%s", v.Name, state)
		if v.Expire != "" {
			fmt.Fprintf(&b, "，到期 %s", v.Expire)
		}
		b.WriteByte('\n')
	}
	if a.Note != "" {
		b.WriteString(a.Note + "\n")
	}
	b.WriteString(formatLibrary(a.Library) + "\n")
	b.WriteString("能否拿到超清母带以 get_song_qualities 的实测为准。")
	return b.String()
}

func formatLibrary(l acquire.LibraryStatus) string {
	switch {
	case !l.Mounted:
		return fmt.Sprintf("曲库：%s（存储没有挂载：SD 卡或读卡器没插上，现在下载会被拒绝）", l.Dir)
	case l.Mount != "":
		return fmt.Sprintf("曲库：%s（SD 卡已插入，剩余 %.1f GB）", l.Dir, l.FreeGB)
	}
	return fmt.Sprintf("曲库：%s（剩余 %.1f GB）", l.Dir, l.FreeGB)
}
