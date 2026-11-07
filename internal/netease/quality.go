package netease

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/XiaoMengXinX/Music163Api-Go/api"
	"github.com/XiaoMengXinX/Music163Api-Go/types"
)

// 网易云音质档位
const (
	LevelStandard = "standard" // 标准
	LevelHigher   = "higher"   // 较高
	LevelExhigh   = "exhigh"   // 极高（320k MP3）
	LevelLossless = "lossless" // 无损（通常 16bit/44.1kHz FLAC）
	LevelHires    = "hires"    // Hi-Res
	LevelJyeffect = "jyeffect" // 高清环绕声
	LevelSky      = "sky"      // 沉浸环绕声
	LevelDolby    = "dolby"    // 杜比全景声
	LevelJymaster = "jymaster" // 超清母带
	LevelVivid    = "vivid"    // 菁彩声 Audio Vivid（三维声）
)

// StereoLevels 是原始立体声混音的档位，从高到低
var StereoLevels = []string{LevelJymaster, LevelHires, LevelLossless, LevelExhigh, LevelHigher, LevelStandard}

// SpatialLevels 是环绕声/空间音频处理版，不是原始立体声混音
var SpatialLevels = []string{LevelVivid, LevelDolby, LevelSky, LevelJyeffect}

func LevelName(level string) string {
	switch level {
	case LevelStandard:
		return "标准"
	case LevelHigher:
		return "较高"
	case LevelExhigh:
		return "极高"
	case LevelLossless:
		return "无损"
	case LevelHires:
		return "Hi-Res"
	case LevelJyeffect:
		return "高清环绕声"
	case LevelSky:
		return "沉浸环绕声"
	case LevelDolby:
		return "杜比全景声"
	case LevelJymaster:
		return "超清母带"
	case LevelVivid:
		return "菁彩声"
	}
	return level
}

// LevelRank 返回立体声档位的高低（越大越高），非立体声档位返回 0
func LevelRank(level string) int {
	for i, l := range StereoLevels {
		if l == level {
			return len(StereoLevels) - i
		}
	}
	return 0
}

func ValidLevel(level string) bool {
	return LevelRank(level) > 0 || slices.Contains(SpatialLevels, level)
}

// SongURL 是请求某个档位时服务器实际给出的文件
type SongURL struct {
	Requested  string            `json:"requested"`
	Level      string            `json:"level"` // 服务器实际给的档位，没权限时会悄悄降档
	URL        string            `json:"-"`
	Type       string            `json:"type"`
	Bitrate    int               `json:"bitrate"`
	Size       int64             `json:"size"`
	MD5        string            `json:"md5"`
	Code       int               `json:"code"`
	Fee        int               `json:"fee"`
	TimeMs     int               `json:"time_ms"`
	SampleRate int               `json:"sample_rate,omitempty"`
	Trial      bool              `json:"trial"` // 只给了试听片段
	TrialFrom  float64           `json:"trial_from,omitempty"`
	TrialTo    float64           `json:"trial_to,omitempty"`
	Raw        types.SongURLData `json:"-"`
}

func (u SongURL) Available() bool { return u.URL != "" }

// SongURL 按指定档位取下载链接，并读出实际档位与试听信息
func (c *Client) SongURL(musicID int, level string) (SongURL, error) {
	res, err := api.GetSongURL(c.requestData(), api.SongURLConfig{Ids: []int{musicID}, Level: level})
	if err != nil {
		return SongURL{}, err
	}
	return parseSongURL(res, level)
}

func parseSongURL(res types.SongsURLData, level string) (SongURL, error) {
	var raw struct {
		Code int `json:"code"`
		Data []struct {
			URL           string          `json:"url"`
			Br            int             `json:"br"`
			Size          int64           `json:"size"`
			MD5           string          `json:"md5"`
			Code          int             `json:"code"`
			Type          string          `json:"type"`
			Fee           int             `json:"fee"`
			Level         any             `json:"level"`
			Time          int             `json:"time"`
			Sr            int             `json:"sr"`
			FreeTrialInfo json.RawMessage `json:"freeTrialInfo"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.RawJson), &raw); err != nil {
		return SongURL{}, fmt.Errorf("解析歌曲链接失败: %w", err)
	}
	if raw.Code != 200 {
		return SongURL{}, fmt.Errorf("获取歌曲链接失败: code %d", raw.Code)
	}
	if len(raw.Data) == 0 || len(res.Data) == 0 {
		return SongURL{}, fmt.Errorf("获取歌曲链接失败: 返回为空")
	}
	d := raw.Data[0]
	u := SongURL{
		Requested:  level,
		URL:        d.URL,
		Type:       strings.ToLower(d.Type),
		Bitrate:    d.Br,
		Size:       d.Size,
		MD5:        strings.ToLower(d.MD5),
		Code:       d.Code,
		Fee:        d.Fee,
		TimeMs:     d.Time,
		SampleRate: d.Sr,
		Raw:        res.Data[0],
	}
	if s, ok := d.Level.(string); ok {
		u.Level = s
	}
	if t := strings.TrimSpace(string(d.FreeTrialInfo)); t != "" && t != "null" {
		u.Trial = true
		var span struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		}
		if json.Unmarshal(d.FreeTrialInfo, &span) == nil {
			u.TrialFrom, u.TrialTo = span.Start, span.End
		}
	}
	return u, nil
}

type Artist struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// FileSpec 是歌曲详情里各档位文件的规格（hr/sq/h/m/l）
type FileSpec struct {
	Bitrate    int   `json:"br"`
	Size       int64 `json:"size"`
	SampleRate int   `json:"sr"`
}

// SongMeta 是下载、打标签、命名所需的歌曲信息
type SongMeta struct {
	ID          int                  `json:"id"`
	Name        string               `json:"name"`
	Artists     []Artist             `json:"artists"`
	Album       string               `json:"album"`
	AlbumID     int                  `json:"album_id"`
	PicURL      string               `json:"pic_url,omitempty"`
	DurationMs  int                  `json:"duration_ms"`
	TrackNo     int                  `json:"track_no"`
	Disc        string               `json:"disc,omitempty"`
	PublishTime int64                `json:"publish_time,omitempty"` // 毫秒
	Alias       []string             `json:"alias,omitempty"`
	Trans       []string             `json:"trans,omitempty"`
	Fee         int                  `json:"fee"`
	MaxLevel    string               `json:"max_level,omitempty"` // 这首歌存在的最高档位
	PlayLevel   string               `json:"play_level,omitempty"`
	Status      int                  `json:"status"`         // 权限状态，小于 0 表示没有版权或已下架
	Tags        []string             `json:"tags,omitempty"` // 如"含AI生成"
	Hr          *FileSpec            `json:"hr,omitempty"`
	Sq          *FileSpec            `json:"sq,omitempty"`
	H           *FileSpec            `json:"h,omitempty"`
	M           *FileSpec            `json:"m,omitempty"`
	L           *FileSpec            `json:"l,omitempty"`
	Detail      types.SongDetailData `json:"-"`
}

// Unavailable 表示网易云不让当前账号/地区播放这首歌（没有版权、已下架或地区限制）
func (m SongMeta) Unavailable() bool { return m.Status < 0 }

// StereoMax 返回立体声档位里能确认存在的最高档：maxBrLevel 是空间音频时退回到 hr/sq/h 推断
func (m SongMeta) StereoMax() string {
	if LevelRank(m.MaxLevel) > 0 {
		return m.MaxLevel
	}
	switch {
	case m.Hr != nil:
		return LevelHires
	case m.Sq != nil:
		return LevelLossless
	case m.H != nil:
		return LevelExhigh
	case m.M != nil:
		return LevelHigher
	case m.L != nil:
		return LevelStandard
	}
	return ""
}

func (m SongMeta) ArtistNames() []string {
	names := make([]string, len(m.Artists))
	for i, a := range m.Artists {
		names[i] = a.Name
	}
	return names
}

// Year 返回发行年份，未知时为 0
func (m SongMeta) Year() int {
	if m.PublishTime <= 0 {
		return 0
	}
	return time.UnixMilli(m.PublishTime).In(time.FixedZone("CST", 8*3600)).Year()
}

// SongMetas 批量获取歌曲信息，结果顺序与 ids 一致，查不到的歌曲被跳过
func (c *Client) SongMetas(ids []int) ([]SongMeta, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	res, err := api.GetSongDetail(c.requestData(), ids)
	if err != nil {
		return nil, err
	}
	return parseSongMetas(res)
}

func (c *Client) SongMeta(musicID int) (SongMeta, error) {
	metas, err := c.SongMetas([]int{musicID})
	if err != nil {
		return SongMeta{}, err
	}
	if len(metas) == 0 {
		return SongMeta{}, fmt.Errorf("找不到歌曲 %d", musicID)
	}
	return metas[0], nil
}

func parseSongMetas(res types.SongsDetailData) ([]SongMeta, error) {
	var raw struct {
		Code  int `json:"code"`
		Songs []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
			Ar   []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"ar"`
			Alia []any `json:"alia"`
			Tns  []any `json:"tns"`
			Al   struct {
				ID     int    `json:"id"`
				Name   string `json:"name"`
				PicURL string `json:"picUrl"`
			} `json:"al"`
			Dt          int       `json:"dt"`
			No          int       `json:"no"`
			Cd          any       `json:"cd"`
			PublishTime int64     `json:"publishTime"`
			Fee         int       `json:"fee"`
			Hr          *FileSpec `json:"hr"`
			Sq          *FileSpec `json:"sq"`
			H           *FileSpec `json:"h"`
			M           *FileSpec `json:"m"`
			L           *FileSpec `json:"l"`
			MarkTags    []struct {
				TagValue string `json:"tagValue"`
			} `json:"markTags"`
		} `json:"songs"`
		Privileges []struct {
			ID             int    `json:"id"`
			St             int    `json:"st"`
			MaxBrLevel     string `json:"maxBrLevel"`
			PlayMaxBrLevel string `json:"playMaxBrLevel"`
		} `json:"privileges"`
	}
	if err := json.Unmarshal([]byte(res.RawJson), &raw); err != nil {
		return nil, fmt.Errorf("解析歌曲信息失败: %w", err)
	}
	if raw.Code != 200 {
		return nil, fmt.Errorf("获取歌曲信息失败: code %d", raw.Code)
	}
	typed := map[int]types.SongDetailData{}
	for _, s := range res.Songs {
		typed[s.Id] = s
	}
	priv := map[int]int{}
	for i, p := range raw.Privileges {
		priv[p.ID] = i
	}
	var out []SongMeta
	for _, s := range raw.Songs {
		m := SongMeta{
			ID:          s.ID,
			Name:        s.Name,
			Album:       s.Al.Name,
			AlbumID:     s.Al.ID,
			PicURL:      s.Al.PicURL,
			DurationMs:  s.Dt,
			TrackNo:     s.No,
			Disc:        strings.TrimSpace(fmt.Sprint(valueOr(s.Cd, ""))),
			PublishTime: s.PublishTime,
			Alias:       strs(s.Alia),
			Trans:       strs(s.Tns),
			Fee:         s.Fee,
			Hr:          s.Hr,
			Sq:          s.Sq,
			H:           s.H,
			M:           s.M,
			L:           s.L,
			Detail:      typed[s.ID],
		}
		for _, a := range s.Ar {
			m.Artists = append(m.Artists, Artist{ID: a.ID, Name: a.Name})
		}
		for _, t := range s.MarkTags {
			if v := strings.TrimSpace(t.TagValue); v != "" {
				m.Tags = append(m.Tags, v)
			}
		}
		if i, ok := priv[s.ID]; ok {
			m.MaxLevel = raw.Privileges[i].MaxBrLevel
			m.PlayLevel = raw.Privileges[i].PlayMaxBrLevel
			m.Status = raw.Privileges[i].St
		}
		out = append(out, m)
	}
	return out, nil
}

func valueOr(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

func strs(v []any) []string {
	var out []string
	for _, x := range v {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// Lyric 返回原文 LRC 与翻译 LRC
func (c *Client) Lyric(musicID int) (lrc, trans string, err error) {
	res, err := api.GetSongLyric(c.requestData(), musicID)
	if err != nil {
		return "", "", err
	}
	return res.Lrc.Lyric, res.Tlyric.Lyric, nil
}

type Account struct {
	LoggedIn bool   `json:"logged_in"`
	UserID   int    `json:"user_id,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	VipType  int    `json:"vip_type,omitempty"`
}

func (c *Client) Account() (Account, error) {
	res, err := api.GetLoginStatus(c.requestData())
	if err != nil {
		return Account{}, err
	}
	return Account{
		LoggedIn: res.Code == 200 && res.Profile.UserId != 0,
		UserID:   res.Profile.UserId,
		Nickname: res.Profile.Nickname,
		VipType:  res.Account.VipType,
	}, nil
}

// VipItem 是一项会员权益，Kind 为接口原始字段名
type VipItem struct {
	Kind   string    `json:"kind"`
	Name   string    `json:"name"`
	Code   int       `json:"code"`
	Expire time.Time `json:"expire"`
	Active bool      `json:"active"`
}

var vipNames = map[string]string{
	"associator":   "黑胶VIP",
	"redplus":      "黑胶SVIP",
	"musicPackage": "音乐包",
	"familyVip":    "家庭会员",
}

// VipStatus 列出账号的各项会员及到期时间
func (c *Client) VipStatus() ([]VipItem, error) {
	res, err := api.GetVipInfo(c.requestData())
	if err != nil {
		return nil, err
	}
	return parseVip(res.RawJson, time.Now())
}

func parseVip(rawJSON string, now time.Time) ([]VipItem, error) {
	var raw struct {
		Code int                        `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &raw); err != nil {
		return nil, fmt.Errorf("解析会员信息失败: %w", err)
	}
	if raw.Code != 200 {
		return nil, fmt.Errorf("获取会员信息失败: code %d", raw.Code)
	}
	var items []VipItem
	for kind, v := range raw.Data {
		var it struct {
			VipCode    *int  `json:"vipCode"`
			ExpireTime int64 `json:"expireTime"`
		}
		if json.Unmarshal(v, &it) != nil || it.VipCode == nil {
			continue
		}
		item := VipItem{Kind: kind, Name: vipNames[kind], Code: *it.VipCode}
		if item.Name == "" {
			item.Name = kind
		}
		if it.ExpireTime > 0 {
			item.Expire = time.UnixMilli(it.ExpireTime)
			item.Active = item.Expire.After(now)
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Kind < items[j].Kind })
	return items, nil
}
