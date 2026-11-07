package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XiaoMengXinX/Music163Api-Go/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hoganyugit/music163-mcp/internal/acquire"
	"github.com/hoganyugit/music163-mcp/internal/audiocheck"
	"github.com/hoganyugit/music163-mcp/internal/config"
	"github.com/hoganyugit/music163-mcp/internal/netease"
	"github.com/hoganyugit/music163-mcp/internal/testaudio"
)

type fakeNetease struct{}

func (fakeNetease) SearchSong(kw string, limit, offset int) (types.SearchSongData, error) {
	var d types.SearchSongData
	if kw == "限流" {
		d.RawJson = `{"code":405,"message":"操作频繁，请稍候再试"}`
		err := json.Unmarshal([]byte(d.RawJson), &d)
		return d, err
	}
	raw := `{"result":{"songs":[{"id":186016,"name":"晴天","artists":[{"id":6452,"name":"周杰伦"}],"album":{"id":18905,"name":"叶惠美"},"duration":269000,"alias":[],"fee":1},
	{"id":5257138,"name":"晴天","artists":[{"id":1,"name":"翻唱者"}],"album":{"id":2,"name":"翻唱集"},"duration":250000,"alias":["Live"],"fee":0}]},"code":200}`
	err := json.Unmarshal([]byte(raw), &d)
	return d, err
}

func (fakeNetease) SongMetas(ids []int) ([]netease.SongMeta, error) {
	return []netease.SongMeta{{ID: 186016, MaxLevel: "vivid", Sq: &netease.FileSpec{SampleRate: 44100}}, {ID: 5257138, MaxLevel: "exhigh", Status: -100, Tags: []string{"含AI生成"}}}, nil
}

func (fakeNetease) Account() (netease.Account, error) {
	return netease.Account{LoggedIn: true, UserID: 42, Nickname: "测试用户"}, nil
}

func (fakeNetease) VipStatus() ([]netease.VipItem, error) {
	return []netease.VipItem{{Kind: "redplus", Name: "黑胶SVIP", Active: true, Expire: time.Date(2027, 1, 2, 0, 0, 0, 0, time.Local)}}, nil
}

// fakeAcquirer 的 Download 在 release 关闭前一直阻塞，用来测后台任务
type fakeAcquirer struct {
	release chan struct{}
	png     string
	mu      sync.Mutex
	calls   int
	stopped bool // Download 因 ctx 取消而收尾
}

func (f *fakeAcquirer) Probe(ctx context.Context, id int, spatial bool) (netease.SongMeta, []acquire.LevelProbe, error) {
	return netease.SongMeta{ID: id, Name: "晴天", Album: "叶惠美", DurationMs: 269000},
		[]acquire.LevelProbe{{Requested: "jymaster", RequestedName: "超清母带", Available: true, Level: "jymaster", Format: "flac", Kbps: 4600, SizeMB: 150}}, nil
}

func (f *fakeAcquirer) LibraryStatus() acquire.LibraryStatus {
	return acquire.LibraryStatus{Dir: "/media/u/CARD/Music/NetEase", Mount: "/media/u/CARD", Mounted: true, FreeGB: 475.2}
}

func (f *fakeAcquirer) Download(ctx context.Context, id int, opt acquire.Options, progress acquire.ProgressFunc) (*acquire.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if id == 404 {
		return nil, errors.New("没有拿到可用的完整文件")
	}
	progress("下载超清母带", 10<<20, 150<<20)
	select {
	case <-f.release:
	case <-ctx.Done():
		time.Sleep(50 * time.Millisecond) // 模拟删除临时文件
		f.mu.Lock()
		f.stopped = true
		f.mu.Unlock()
		return nil, ctx.Err()
	}
	progress("写入标签和封面", 0, 0)
	return &acquire.Result{
		MusicID: id, Title: "晴天", Artists: "周杰伦", Album: "叶惠美", Level: "lossless", LevelName: "无损",
		Path: "/lib/周杰伦/叶惠美/03 晴天.flac", SizeMB: 30, Decision: "改存无损",
		Report:   &audiocheck.Report{Verdict: audiocheck.VerdictGenuine, Summary: "未发现造假痕迹", EffectiveSpec: "16bit/44.1kHz", Spectrogram: f.png},
		Attempts: []acquire.Attempt{{Requested: "jymaster", Level: "jymaster", LevelName: "超清母带", Status: "replaced", Verdict: audiocheck.VerdictUpsampled}},
	}, nil
}

type testEnv struct {
	srv      *Server
	cancel   context.CancelFunc
	cs       *mcp.ClientSession
	acq      *fakeAcquirer
	cfg      *config.Config
	root     string
	progress chan string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.Netease.MUSICU = "x"
	cfg.Library.Dir = filepath.Join(root, "lib")
	cfg.Library.StateDir = filepath.Join(root, "state")
	cfg.Library.CheckDirs = []string{filepath.Join(root, "music")}
	for _, d := range []string{cfg.Library.Dir, cfg.Library.StateDir, cfg.Library.CheckDirs[0], filepath.Join(root, "outside")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	png := filepath.Join(root, "state", "x.png")
	if err := os.WriteFile(png, []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	acq := &fakeAcquirer{release: make(chan struct{}), png: png}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(ctx, Deps{Config: cfg, Netease: fakeNetease{}, Acquire: acq, Version: "test",
		ResolveShortLink: func(s string) string { return "https://music.163.com/song?id=186016" }})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	progress := make(chan string, 100)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			select {
			case progress <- req.Params.Message:
			default:
			}
		},
	})
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &testEnv{srv: srv, cancel: cancel, cs: cs, acq: acq, cfg: cfg, root: root, progress: progress}
}

func (e *testEnv) call(t *testing.T, name string, args map[string]any, token any) *mcp.CallToolResult {
	t.Helper()
	p := &mcp.CallToolParams{Name: name, Arguments: args}
	if token != nil {
		p.SetProgressToken(token)
	}
	res, err := e.cs.CallTool(context.Background(), p)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func structured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	data, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatal(err)
	}
}

func TestToolsListed(t *testing.T) {
	e := newTestEnv(t)
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.InputSchema == nil || tool.Description == "" {
			t.Errorf("tool %s missing schema/description", tool.Name)
		}
	}
	for _, name := range []string{"search_songs", "get_song_qualities", "download_song", "job_status", "check_audio", "account_status"} {
		if !got[name] {
			t.Errorf("missing tool %s", name)
		}
	}
}

func TestSearchAndAccount(t *testing.T) {
	e := newTestEnv(t)
	res := e.call(t, "search_songs", map[string]any{"keyword": "晴天 周杰伦"}, nil)
	if res.IsError || !strings.Contains(text(res), "id=186016") || !strings.Contains(text(res), "立体声最高：无损；另有菁彩声空间音频版") ||
		!strings.Contains(text(res), "[无版权或地区限制，下载不了] [含AI生成]") {
		t.Fatalf("search: %s", text(res))
	}
	var out searchOut
	structured(t, res, &out)
	if len(out.Results) != 2 || out.Results[1].MaxLevel != "exhigh" || out.Results[1].Alias[0] != "Live" || !out.Results[1].Unavailable || out.Results[0].StereoMax != "lossless" {
		t.Fatalf("structured: %+v", out)
	}
	res = e.call(t, "account_status", map[string]any{}, nil)
	if res.IsError || !strings.Contains(text(res), "黑胶SVIP：生效中，到期 2027-01-02") || strings.Contains(text(res), "MUSIC_U") ||
		!strings.Contains(text(res), "SD 卡已插入，剩余 475.2 GB") {
		t.Fatalf("account: %s", text(res))
	}
	res = e.call(t, "get_song_qualities", map[string]any{"song": "https://163cn.tv/abc"}, nil)
	if res.IsError || !strings.Contains(text(res), "超清母带：FLAC 4600 kbps") {
		t.Fatalf("qualities: %s", text(res))
	}
	res = e.call(t, "get_song_qualities", map[string]any{"song": "晴天"}, nil)
	if !res.IsError || !strings.Contains(text(res), "search_songs") {
		t.Fatalf("keyword input should be rejected: %s", text(res))
	}
}

func TestSearchThrottledIsAnError(t *testing.T) {
	e := newTestEnv(t)
	res := e.call(t, "search_songs", map[string]any{"keyword": "限流"}, nil)
	if !res.IsError || strings.Contains(text(res), "没有搜到") || !strings.Contains(text(res), "code 405") ||
		!strings.Contains(text(res), "操作频繁") {
		t.Fatalf("throttled search: %s", text(res))
	}
}

func TestDrainWaitsForCancelledJobs(t *testing.T) {
	e := newTestEnv(t)
	res := e.call(t, "download_song", map[string]any{"song": "186016", "wait_seconds": 0}, nil)
	var out jobOut
	structured(t, res, &out)
	if out.Status != "running" {
		t.Fatalf("status %s: %s", out.Status, text(res))
	}
	e.cancel()
	if !e.srv.Drain(5 * time.Second) {
		t.Fatal("drain timed out")
	}
	e.acq.mu.Lock()
	defer e.acq.mu.Unlock()
	if !e.acq.stopped {
		t.Fatal("Drain returned before the job finished cleaning up")
	}
}

func TestDownloadJobLifecycle(t *testing.T) {
	e := newTestEnv(t)
	res := e.call(t, "download_song", map[string]any{"song": "186016", "wait_seconds": 1}, nil)
	var out jobOut
	structured(t, res, &out)
	if out.Status != "running" || out.JobID == "" || !strings.Contains(out.Progress, "下载超清母带") {
		t.Fatalf("first call: %+v %s", out, text(res))
	}
	// 同一首歌再次请求复用同一个任务
	res = e.call(t, "download_song", map[string]any{"song": "https://music.163.com/song?id=186016", "wait_seconds": 0}, nil)
	var again jobOut
	structured(t, res, &again)
	if again.JobID != out.JobID {
		t.Fatalf("job not reused: %s vs %s", again.JobID, out.JobID)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(e.acq.release)
	}()
	res = e.call(t, "job_status", map[string]any{"job_id": out.JobID, "wait_seconds": 10}, "tok-1")
	var done jobOut
	structured(t, res, &done)
	if done.Status != "done" || done.Result == nil || done.Result.Level != "lossless" {
		t.Fatalf("job_status: %+v", done)
	}
	var images int
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok && img.MIMEType == "image/png" {
			images++
		}
	}
	if images != 1 || !strings.Contains(text(res), "选择理由：改存无损") || !strings.Contains(text(res), "超清母带：已换掉，升频") {
		t.Fatalf("result content: images=%d %s", images, text(res))
	}
	select {
	case msg := <-e.progress:
		if !strings.Contains(msg, "写入标签") {
			t.Errorf("unexpected progress %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Error("no progress notification received")
	}
	if e.acq.calls != 1 {
		t.Errorf("Download called %d times", e.acq.calls)
	}

	res = e.call(t, "download_song", map[string]any{"song": "404", "wait_seconds": 5}, nil)
	if !res.IsError || !strings.Contains(text(res), "没有拿到") {
		t.Fatalf("failure not reported: %s", text(res))
	}
	res = e.call(t, "download_song", map[string]any{"song": "1", "level": "ultra"}, nil)
	if !res.IsError {
		t.Fatal("bad level accepted")
	}
	res = e.call(t, "job_status", map[string]any{"job_id": "dl-nope"}, nil)
	if !res.IsError {
		t.Fatal("unknown job accepted")
	}
}

func TestCheckAudioPathWhitelist(t *testing.T) {
	e := newTestEnv(t)
	inside := filepath.Join(e.cfg.Library.CheckDirs[0], "a.flac")
	if err := testaudio.MakeFLAC(inside, 44100, 16, 6, testaudio.Lowpass(testaudio.MusicLevel, 16000, 300), false); err != nil {
		t.Fatal(err)
	}
	res := e.call(t, "check_audio", map[string]any{"path": inside}, nil)
	if res.IsError || !strings.Contains(text(res), "lossy_likely") {
		t.Fatalf("check: %s", text(res))
	}
	var rep audiocheck.Report
	structured(t, res, &rep)
	if rep.Verdict != audiocheck.VerdictLossyLikely || rep.Spectrogram == "" {
		t.Fatalf("report: %+v", rep)
	}
	var hasImage bool
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			hasImage = true
		}
	}
	if !hasImage {
		t.Error("spectrogram image not returned")
	}

	outside := filepath.Join(e.root, "outside", "b.flac")
	if err := os.WriteFile(outside, []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(e.cfg.Library.CheckDirs[0], "link.flac")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, link, "relative/a.flac", filepath.Join(e.cfg.Library.CheckDirs[0], "..", "outside", "b.flac")} {
		res := e.call(t, "check_audio", map[string]any{"path": p}, nil)
		if !res.IsError {
			t.Errorf("path %s should be rejected: %s", p, text(res))
		}
	}
}
