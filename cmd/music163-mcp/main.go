// music163-mcp：网易云音乐 MCP 服务（下载真正最高音质 + 假无损检测）
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
	"github.com/skip2/go-qrcode"

	"github.com/hoganyugit/music163-mcp/internal/acquire"
	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/config"
	"github.com/hoganyugit/music163-mcp/internal/fetch"
	"github.com/hoganyugit/music163-mcp/internal/logging"
	"github.com/hoganyugit/music163-mcp/internal/mcpserver"
	"github.com/hoganyugit/music163-mcp/internal/netease"
)

const (
	apiInterval = 300 * time.Millisecond
	// drainTimeout 是退出前等后台下载停下并删除临时文件的最长时间
	drainTimeout = 15 * time.Second
)

var (
	versionName = "v0.1.0"
	commitSHA   = ""
)

// out 是真正的标准输出：serve 模式下只给 MCP 协议用，其余命令用来打印结果。
// 启动时把 os.Stdout 换成 stderr，任何库的误打印都不会弄坏协议流。
var out io.Writer

const usage = `music163-mcp —— 网易云音乐 MCP 服务：下载真正最高音质，识别假无损

用法：
  music163-mcp init      [-c 配置]                    只生成配置文件（权限 600）
  music163-mcp login     [-c 配置] [-force]           终端扫码登录（首次运行会生成配置文件）
  music163-mcp serve     [-c 配置]                    以 stdio 方式运行 MCP 服务（给 agent 用）
  music163-mcp status    [-c 配置]                    查看登录和会员状态
  music163-mcp search    [-c 配置] [-n 10] 关键词     搜索歌曲
  music163-mcp qualities [-c 配置] [-spatial] 歌曲    查询各档位实际能拿到什么（不下载）
  music163-mcp download  [-c 配置] [-level auto] [-force] 歌曲   下载并检测
  music163-mcp check     [-png 输出.png] [-json] 文件 检测本地音频真伪
  music163-mcp version

"歌曲"可以是歌曲 ID 或网易云歌曲链接。默认配置文件：%s
`

func main() {
	out = os.Stdout
	os.Stdout = os.Stderr

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, config.DefaultPath())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "login":
		err = runLogin(args)
	case "init":
		err = runInit(args)
	case "status":
		err = runTool(args, "status")
	case "search":
		err = runTool(args, "search")
	case "qualities":
		err = runTool(args, "qualities")
	case "download":
		err = runTool(args, "download")
	case "check":
		err = runCheck(args)
	case "version", "-v", "--version":
		fmt.Fprintf(out, "music163-mcp %s %s\n", versionName, commitSHA)
	case "help", "-h", "--help":
		fmt.Fprintf(out, usage, config.DefaultPath())
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n"+usage, cmd, config.DefaultPath())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

type app struct {
	cfg      *config.Config
	path     string
	log      *logrus.Logger
	closeLog func() error
	api      *netease.Client
	acq      *acquire.Service
}

func loadApp(path string) (*app, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("配置文件 %s 不存在，请先运行 music163-mcp login", path)
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "警告：配置文件 %s 包含登录凭据，但其他用户可读，建议 chmod 600\n", path)
	}
	log, closeLog := logging.Setup(cfg.Log.Level, cfg.Log.Path)
	client, err := cfg.Netease.HTTPClient()
	if err != nil {
		return nil, err
	}
	api := netease.New(cfg.Netease.MUSICU, client)
	// 所有接口请求（搜索、详情、链接、歌词、账号）至少间隔 300ms，同一台机器上的多个进程共享这个间隔
	api.SetGate(netease.NewGate(apiInterval, filepath.Join(cfg.Library.StateDir, "api.lock")))
	dl := fetch.New(acquire.IncomingDir(cfg.Library), cfg.Download.Timeout.D(), cfg.Download.Retries, cfg.Download.ReverseProxy, cfg.Download.AutoRetry)
	acq, err := acquire.New(api, dl, cfg.Library, cfg.Download.Concurrency, log)
	if err != nil {
		return nil, err
	}
	return &app{cfg: cfg, path: path, log: log, closeLog: closeLog, api: api, acq: acq}, nil
}

func (a *app) deps() mcpserver.Deps {
	return mcpserver.Deps{Config: a.cfg, Netease: a.api, Acquire: a.acq, Log: a.log, Version: versionName}
}

func runServe(args []string) error {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fset.String("c", config.DefaultPath(), "配置文件")
	_ = fset.Parse(args)
	// 还没有配置时先生成模板，让服务能启动；登录后会自动生效
	if _, err := os.Stat(*path); errors.Is(err, fs.ErrNotExist) {
		if err := config.WriteTemplate(*path); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "已创建配置文件 %s\n", *path)
	}
	a, err := loadApp(*path)
	if err != nil {
		return err
	}
	defer a.closeLog()
	if a.cfg.Netease.MUSICU == "" {
		a.log.Warnln("还没有登录，请在终端运行 music163-mcp login 扫码，登录后无需重启即可生效")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go watchLogin(ctx, *path, a.api, a.log, a.cfg.Netease.MUSICU)
	jobCtx, cancelJobs := context.WithCancel(ctx)
	srv := mcpserver.New(jobCtx, a.deps())
	defer shutdown(stop, cancelJobs, srv, a.log)
	a.log.Infof("music163-mcp %s 已启动，曲库 %s", versionName, a.cfg.Library.Dir)
	stdout, ok := out.(*os.File)
	if !ok {
		return errors.New("标准输出不可用")
	}
	err = srv.MCP().Run(ctx, &mcp.IOTransport{Reader: os.Stdin, Writer: stdout})
	if err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// runTool 在进程内连接 MCP 服务调用工具，输出与 agent 看到的完全一致
func runTool(args []string, cmd string) error {
	fset := flag.NewFlagSet(cmd, flag.ExitOnError)
	path := fset.String("c", config.DefaultPath(), "配置文件")
	limit := fset.Int("n", 10, "search：返回条数")
	spatial := fset.Bool("spatial", false, "qualities：同时查询空间音频档位")
	level := fset.String("level", "auto", "download：auto 或指定档位")
	force := fset.Bool("force", false, "download：已下载过也重新下载")
	_ = fset.Parse(args)
	rest := strings.TrimSpace(strings.Join(fset.Args(), " "))

	var name string
	var toolArgs map[string]any
	switch cmd {
	case "status":
		name, toolArgs = "account_status", map[string]any{}
	case "search":
		name, toolArgs = "search_songs", map[string]any{"keyword": rest, "limit": *limit}
	case "qualities":
		name, toolArgs = "get_song_qualities", map[string]any{"song": rest, "include_spatial": *spatial}
	case "download":
		name, toolArgs = "download_song", map[string]any{"song": rest, "level": *level, "force": *force, "wait_seconds": 1800}
	}
	if cmd != "status" && rest == "" {
		return fmt.Errorf("缺少参数，用法见 music163-mcp help")
	}
	a, err := loadApp(*path)
	if err != nil {
		return err
	}
	defer a.closeLog()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	jobCtx, cancelJobs := context.WithCancel(ctx)
	srv := mcpserver.New(jobCtx, a.deps())
	defer shutdown(stop, cancelJobs, srv, a.log)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		return err
	}
	// 终端里原地刷新进度；输出被重定向时（例如 agent 调用）逐行打印
	var lastLen int
	tty := false
	if st, err := os.Stderr.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		tty = true
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "music163-mcp-cli", Version: versionName}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			msg := req.Params.Message
			if !tty {
				fmt.Fprintln(os.Stderr, msg)
				return
			}
			fmt.Fprintf(os.Stderr, "\r%s%s", msg, strings.Repeat(" ", max(0, lastLen-len(msg))))
			lastLen = len(msg)
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	p := &mcp.CallToolParams{Name: name, Arguments: toolArgs}
	p.SetProgressToken("cli")
	res, err := cs.CallTool(ctx, p)
	if lastLen > 0 {
		fmt.Fprintln(os.Stderr)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "已取消，正在停止下载并清理临时文件（再按一次 Ctrl+C 立即退出）…")
		return errors.New("已取消")
	}
	if err != nil {
		return err
	}
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			fmt.Fprintln(out, t.Text)
		}
	}
	if res.IsError {
		return errors.New("执行失败")
	}
	return nil
}

// shutdown 在退出前取消还在跑的下载，并等它们删除临时文件。
// 先恢复默认信号处理，等待期间再按一次 Ctrl+C 可以立即退出
func shutdown(stop, cancelJobs context.CancelFunc, srv *mcpserver.Server, log *logrus.Logger) {
	stop()
	cancelJobs()
	if !srv.Drain(drainTimeout) {
		log.Warnf("有下载任务没能在 %s 内停下，残留的临时文件会在 24 小时后的启动时清理", drainTimeout)
	}
}

func runCheck(args []string) error {
	fset := flag.NewFlagSet("check", flag.ExitOnError)
	png := fset.String("png", "", "频谱图输出路径（不填则不生成）")
	asJSON := fset.Bool("json", false, "输出完整 JSON 报告")
	dur := fset.Float64("duration", 0, "官方时长（秒），用于识别试听片段")
	_ = fset.Parse(args)
	if fset.NArg() != 1 {
		return fmt.Errorf("用法：music163-mcp check [-png 输出.png] [-json] 文件")
	}
	rep, err := audiocheck.Analyze(fset.Arg(0), audiocheck.Options{
		Expect:      audiocheck.Expect{DurationMs: int(*dur * 1000)},
		Spectrogram: *png,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(rep)
	}
	fmt.Fprintln(out, mcpserver.FormatReport(rep))
	return nil
}

// watchLogin 发现配置文件里的登录凭据变化后立即生效，扫码登录后不用重启 agent
func watchLogin(ctx context.Context, path string, api *netease.Client, log *logrus.Logger, current string) {
	var lastMod time.Time
	if st, err := os.Stat(path); err == nil {
		lastMod = st.ModTime()
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().After(lastMod) {
			continue
		}
		lastMod = st.ModTime()
		cfg, err := config.Load(path)
		if err != nil {
			log.Warnf("重新读取配置失败: %v", err)
			continue
		}
		if cfg.Netease.MUSICU != current {
			api.SetMUSICU(cfg.Netease.MUSICU)
			current = cfg.Netease.MUSICU
			log.Infoln("检测到新的登录凭据，已生效")
		}
	}
}

func runInit(args []string) error {
	fset := flag.NewFlagSet("init", flag.ExitOnError)
	path := fset.String("c", config.DefaultPath(), "配置文件")
	_ = fset.Parse(args)
	if err := config.WriteTemplate(*path); err != nil {
		return err
	}
	fmt.Fprintf(out, "已创建配置文件 %s（权限 600）。下一步：music163-mcp login 扫码登录\n", *path)
	return nil
}

func runLogin(args []string) error {
	fset := flag.NewFlagSet("login", flag.ExitOnError)
	path := fset.String("c", config.DefaultPath(), "配置文件")
	force := fset.Bool("force", false, "已登录时也重新扫码（切换账号）")
	_ = fset.Parse(args)

	if _, err := os.Stat(*path); errors.Is(err, fs.ErrNotExist) {
		if err := config.WriteTemplate(*path); err != nil {
			return err
		}
		fmt.Fprintf(out, "已创建配置文件 %s（权限 600），曲库目录等设置可以在里面修改\n", *path)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	client, err := cfg.Netease.HTTPClient()
	if err != nil {
		return err
	}
	if cfg.Netease.MUSICU != "" && !*force {
		acc, err := netease.New(cfg.Netease.MUSICU, client).Account()
		if err == nil && acc.LoggedIn {
			fmt.Fprintf(out, "已经登录为 %s，无需重新登录（切换账号请加 -force）\n", acc.Nickname)
			return nil
		}
		fmt.Fprintln(out, "保存的登录已失效，重新扫码登录")
	}

	api := netease.New("", client)
	key, err := api.CreateQRLoginKey()
	if err != nil {
		return fmt.Errorf("获取登录二维码失败: %w", err)
	}
	qr, err := qrcode.New(netease.QRLoginContent(key), qrcode.Medium)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\n请用网易云音乐 App 扫描下面的二维码，然后在手机上确认登录：")
	fmt.Fprint(out, qr.ToSmallString(false))
	fmt.Fprintln(out, "二维码 3 分钟内有效。按 Ctrl+C 取消。")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	deadline := time.After(3 * time.Minute)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	scanned, failures := false, 0
	for {
		select {
		case <-ctx.Done():
			return errors.New("已取消")
		case <-deadline:
			return errors.New("二维码已过期，请重新运行 music163-mcp login")
		case <-tick.C:
		}
		status, nickname, musicU, err := api.CheckQRLogin(key)
		if err != nil {
			if failures++; failures >= 10 {
				return fmt.Errorf("查询扫码状态失败: %w", err)
			}
			continue
		}
		failures = 0
		switch status {
		case netease.QRScanned:
			if !scanned {
				scanned = true
				fmt.Fprintf(out, "已扫码（%s），请在手机上确认…\n", nickname)
			}
		case netease.QRExpired:
			return errors.New("二维码已过期，请重新运行 music163-mcp login")
		case netease.QRSuccess:
			if err := config.UpdateMUSICU(*path, musicU); err != nil {
				return fmt.Errorf("保存登录凭据失败: %w", err)
			}
			if err := os.Chmod(*path, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(out, "登录成功：%s。登录凭据已保存到 %s（权限 600），不会出现在 agent 的对话里。\n", nickname, *path)
			if items, err := netease.New(musicU, client).VipStatus(); err == nil {
				for _, it := range items {
					if it.Active {
						fmt.Fprintf(out, "会员：%s，到期 %s\n", it.Name, it.Expire.Format("2006-01-02"))
					}
				}
			}
			return nil
		}
	}
}
