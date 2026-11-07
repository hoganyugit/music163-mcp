// Package config 加载并校验 music163-mcp 的 TOML 配置
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/sirupsen/logrus"
)

// Duration 时长类型，接受 "60s" 这类格式
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

type NeteaseConfig struct {
	MUSICU string `toml:"music_u"`
	Proxy  string `toml:"proxy"`
}

func (c NeteaseConfig) proxyURL() (*url.URL, error) {
	if c.Proxy == "" {
		return nil, nil
	}
	invalid := fmt.Errorf("配置项 [netease] proxy 必须为 http/https/socks5/socks5h 代理 URL，主机名不能为空，端口须为 1..65535，不允许路径、query 或 fragment")
	u, err := url.Parse(c.Proxy)
	if err != nil {
		return nil, invalid
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, invalid
	}
	if u.Hostname() == "" || u.Opaque != "" ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") ||
		u.ForceQuery || u.RawQuery != "" || strings.Contains(c.Proxy, "#") ||
		strings.HasSuffix(u.Host, ":") ||
		(strings.Contains(u.Hostname(), ":") && !strings.HasPrefix(u.Host, "[")) {
		return nil, invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, invalid
		}
	}
	return u, nil
}

// HTTPClient 要求配置已校验；空代理保留默认传输行为
func (c NeteaseConfig) HTTPClient() (*http.Client, error) {
	client := &http.Client{}
	if c.Proxy == "" {
		return client, nil
	}
	u, err := url.Parse(c.Proxy)
	if err != nil {
		return nil, fmt.Errorf("配置项 [netease] proxy 不是合法代理 URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(u)
	client.Transport = transport
	return client, nil
}

type DownloadConfig struct {
	Timeout      Duration `toml:"timeout"`
	Retries      int      `toml:"retries"`
	Concurrency  int      `toml:"concurrency"`
	ReverseProxy string   `toml:"reverse_proxy"`
	AutoRetry    bool     `toml:"auto_retry"`
}

type LogConfig struct {
	Level string `toml:"level"`
	Path  string `toml:"path"`
}

// LibraryConfig 是曲库与状态目录配置
type LibraryConfig struct {
	Dir               string   `toml:"dir"`                // 曲库目录：歌手/专辑/曲号 歌名.flac
	StateDir          string   `toml:"state_dir"`          // 临时下载、检测报告、频谱图
	CheckDirs         []string `toml:"check_dirs"`         // check_audio 允许读取的目录（曲库目录总是允许）
	Lyrics            bool     `toml:"lyrics"`             // 保存同名 .lrc
	LyricsTranslation bool     `toml:"lyrics_translation"` // 歌词附带翻译
	CoverMaxPx        int      `toml:"cover_max_px"`       // 内嵌封面最长边，0 表示保留原图
	// Mount 是曲库所在移动存储（如 SD 卡）的挂载点；设置后没插卡时拒绝下载，避免写到电脑硬盘上
	Mount string `toml:"mount"`
}

// Config 是 music163-mcp 的全部配置
type Config struct {
	Netease  NeteaseConfig  `toml:"netease"`
	Download DownloadConfig `toml:"download"`
	Library  LibraryConfig  `toml:"library"`
	Log      LogConfig      `toml:"log"`
}

func Default() *Config {
	return &Config{
		Download: DownloadConfig{
			Timeout:     Duration(300 * time.Second),
			Retries:     3,
			Concurrency: 2,
			AutoRetry:   true,
		},
		Library: LibraryConfig{
			Dir:        "~/Music/NetEase",
			StateDir:   "~/.local/share/music163-mcp",
			CheckDirs:  []string{"~/Music"},
			Lyrics:     true,
			CoverMaxPx: 1200,
		},
		Log: LogConfig{Level: "info"},
	}
}

// DefaultPath 为 $XDG_CONFIG_HOME/music163/mcp.toml，默认 ~/.config/music163/mcp.toml
func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "music163", "mcp.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp.toml"
	}
	return filepath.Join(home, ".config", "music163", "mcp.toml")
}

// Load 严格解析配置，展开 ~ 并把相对路径解释为相对配置文件所在目录
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	base := filepath.Dir(path)
	lib := &cfg.Library
	for _, p := range []*string{&lib.Dir, &lib.StateDir, &lib.Mount, &cfg.Log.Path} {
		if *p == "" {
			continue
		}
		if *p, err = resolvePath(*p, base); err != nil {
			return nil, err
		}
	}
	for i := range lib.CheckDirs {
		if lib.CheckDirs[i], err = resolvePath(lib.CheckDirs[i], base); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolvePath(p, base string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法展开 %q: %w", p, err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

// Validate 允许 music_u 为空，由 login 命令引导登录
func (c *Config) Validate() error {
	if _, err := c.Netease.proxyURL(); err != nil {
		return err
	}
	if c.Download.Timeout <= 0 {
		return fmt.Errorf("配置项 [download] timeout 必须为正时长")
	}
	if c.Download.Retries < 0 {
		return fmt.Errorf("配置项 [download] retries 不能为负数")
	}
	if c.Download.Concurrency <= 0 || c.Download.Concurrency > 8 {
		return fmt.Errorf("配置项 [download] concurrency 必须在 1..8 之间")
	}
	if c.Library.Dir == "" {
		return fmt.Errorf("配置项 [library] dir 不能为空")
	}
	if c.Library.StateDir == "" {
		return fmt.Errorf("配置项 [library] state_dir 不能为空")
	}
	if c.Library.Dir == filepath.Dir(c.Library.Dir) {
		return fmt.Errorf("配置项 [library] dir 不能是根目录")
	}
	if m := c.Library.Mount; m != "" {
		if rel, err := filepath.Rel(m, c.Library.Dir); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("配置项 [library] dir 必须在 mount（%s）里面", m)
		}
	}
	if c.Library.CoverMaxPx != 0 && c.Library.CoverMaxPx < 300 {
		return fmt.Errorf("配置项 [library] cover_max_px 应为 0（保留原图）或不小于 300")
	}
	if _, err := logrus.ParseLevel(c.Log.Level); err != nil {
		return fmt.Errorf("配置项 [log] level %q 不合法", c.Log.Level)
	}
	return nil
}

// ReadDirs 是 check_audio 允许读取的目录
func (c *Config) ReadDirs() []string {
	return append([]string{c.Library.Dir}, c.Library.CheckDirs...)
}

const configTemplate = `# music163-mcp 配置文件
# 这个文件保存了网易云的登录凭据，权限应保持为 600，不要分享给别人

[netease]
# 登录凭据，由 ` + "`music163-mcp login`" + ` 扫码后自动写入
music_u = ''
# 可选：访问网易云接口用的代理。机器在海外时需要填国内代理，如 'http://127.0.0.1:7890'
proxy = ''

[download]
# 单次下载尝试的时长上限，超时后断点续传重试
timeout = '300s'
retries = 3
# 同时下载的歌曲数
concurrency = 2
reverse_proxy = ''
auto_retry = true

[library]
# 曲库目录，文件按 歌手/专辑/曲号 歌名.flac 存放，可整个拷进 Walkman 的 MUSIC 文件夹
dir = '~/Music/NetEase'
# 曲库在 SD 卡等移动存储上时，填它的挂载点（如 '/media/用户名/XXXX-XXXX'）：
# 没插卡时会拒绝下载，不会把歌写到电脑硬盘上。曲库在本机硬盘时留空
mount = ''
# 临时下载、检测报告和频谱图放在这里，不会混进曲库
state_dir = '~/.local/share/music163-mcp'
# check_audio 允许检测的目录（曲库目录总是允许）
check_dirs = ['~/Music']
# 保存同名 .lrc 歌词（Walkman 可直接显示）
lyrics = true
# 歌词是否附带中文翻译
lyrics_translation = false
# 内嵌封面最长边像素，0 表示保留原图
cover_max_px = 1200

[log]
level = 'info'
# 日志文件目录，留空则只输出到 stderr
path = ''
`

// WriteTemplate 新建权限为 600 的配置模板，文件已存在时报错
func WriteTemplate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("配置文件 %s 已存在", path)
		}
		return err
	}
	if _, err := f.WriteString(configTemplate); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
