package config

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTemplateLoadsAndUpdates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "music163", "mcp.toml")
	if err := WriteTemplate(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteTemplate(path); err == nil {
		t.Fatal("overwrite should fail")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v", st.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Library.Dir != filepath.Join(home, "Music", "NetEase") || cfg.Library.StateDir != filepath.Join(home, ".local", "share", "music163-mcp") {
		t.Fatalf("paths not expanded: %+v", cfg.Library)
	}
	if cfg.Download.Timeout.D() != 300*time.Second || cfg.Download.Concurrency != 2 || !cfg.Library.Lyrics || cfg.Library.CoverMaxPx != 1200 {
		t.Fatalf("defaults wrong: %+v %+v", cfg.Download, cfg.Library)
	}
	if got := cfg.ReadDirs(); len(got) != 2 || got[1] != filepath.Join(home, "Music") {
		t.Fatalf("read dirs = %v", got)
	}

	if err := UpdateMUSICU(path, "SECRET"); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Netease.MUSICU != "SECRET" {
		t.Fatalf("music_u = %q", cfg.Netease.MUSICU)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("perm changed to %v", st.Mode().Perm())
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got := DefaultPath(); got != filepath.Join("/tmp/xdg", "music163", "mcp.toml") {
		t.Errorf("DefaultPath with XDG_CONFIG_HOME = %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/home")
	if got := DefaultPath(); got != filepath.Join("/tmp/home", ".config", "music163", "mcp.toml") {
		t.Errorf("DefaultPath = %q", got)
	}
}

func TestRelativePathsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[library]\ndir = 'lib'\nstate_dir = 'state'\ncheck_dirs = []\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Library.Dir != filepath.Join(dir, "lib") || cfg.Library.StateDir != filepath.Join(dir, "state") {
		t.Fatalf("relative paths: %+v", cfg.Library)
	}
	for _, bad := range []string{
		"[bot]\ntoken = 'x'\n", // 原 Telegram bot 的配置段，严格模式下拒绝
		"[netease]\nlevel = 'hires'\n",
		"badkey = 1\n",
		"[download]\nconcurrency = 0\n",
		"[download]\ntimeout = '0s'\n",
		"[download]\nretries = -1\n",
		"[library]\ncover_max_px = 100\n",
		"[library]\ndir = '/'\n",
		"[netease]\nproxy = 'ftp://x'\n",
		"[library]\ndir = '/data/music'\nmount = '/media/u/CARD'\n",
		"[library]\ndir = '/media/u/CARD'\nmount = '/media/u/CARD'\n",
		"[log]\nlevel = 'loud'\n",
	} {
		write(bad)
		if _, err := Load(path); err == nil {
			t.Errorf("expected error for %q", strings.TrimSpace(bad))
		}
	}
}

func TestMountInsideOK(t *testing.T) {
	path := writeTemp(t, "[library]\ndir = '/media/u/CARD/Music/NetEase'\nmount = '/media/u/CARD/'\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Library.Mount != "/media/u/CARD" {
		t.Fatalf("mount = %q", cfg.Library.Mount)
	}
}

func TestUpdateMUSICU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.toml")
	original := `# 我的配置注释
[netease]
# music_u 注释
music_u = 'old-cookie' # 行尾注释
proxy = ''

[download]
timeout = "60s"

[library]
dir = 'lib'
state_dir = 'state'
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := UpdateMUSICU(path, "new-cookie<>&"); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `# 我的配置注释
[netease]
# music_u 注释
music_u = "new-cookie<>&" # 行尾注释
proxy = ''

[download]
timeout = "60s"

[library]
dir = 'lib'
state_dir = 'state'
`
	if string(after) != want {
		t.Errorf("回写结果不符合预期:\n%s", after)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Netease.MUSICU != "new-cookie<>&" {
		t.Errorf("music_u = %q", cfg.Netease.MUSICU)
	}
	if cfg.Download.Timeout.D() != 60*time.Second || cfg.Library.Dir != filepath.Join(dir, "lib") {
		t.Errorf("其它配置被误改: %+v", cfg)
	}

	// 同时覆盖单引号和双引号赋值
	if err := UpdateMUSICU(path, "third"); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Netease.MUSICU != "third" {
		t.Errorf("二次更新失败: %q", cfg.Netease.MUSICU)
	}
}

func TestUpdateMUSICUMissingKey(t *testing.T) {
	path := writeTemp(t, "[library]\ndir = 'lib'\n")
	if err := UpdateMUSICU(path, "y"); err == nil {
		t.Error("缺少 music_u 应报错")
	}
}

func TestNeteaseProxyValidation(t *testing.T) {
	for _, proxy := range []string{
		"", "http://localhost", "https://localhost:443/", "socks5://localhost:1080",
		"socks5h://localhost", "http://user:password@proxy.example:1",
		"socks5://user:p%40ssword@127.0.0.1:65535", "http://[::1]", "http://[::1]:8080/",
	} {
		t.Run("valid/"+proxy, func(t *testing.T) {
			cfg := Default()
			cfg.Netease.Proxy = proxy
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, proxy := range []string{
		"localhost:8080", "//localhost:8080", "ftp://localhost", "http:localhost",
		"http://", "http://:8080", "http://user:password@:8080", "http:///localhost",
		"http://localhost:0", "http://localhost:65536", "http://localhost:-1",
		"http://localhost:abc", "http://localhost:", "http://[::1]:", "http://::1",
		"http://localhost/path", "http://localhost//", "http://localhost/%2F", "http://localhost/%2f",
		"http://localhost?query", "http://localhost?", "http://localhost#fragment", "http://localhost#",
		"http://user:password@localhost:bad", "http://user:password@localhost/%zz", "http://user:password@bad host",
	} {
		t.Run("invalid/"+proxy, func(t *testing.T) {
			cfg := Default()
			cfg.Netease.Proxy = proxy
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected invalid proxy error")
			}
			if !strings.Contains(err.Error(), "[netease] proxy") || strings.Contains(err.Error(), proxy) || strings.Contains(err.Error(), "password") {
				t.Fatalf("unexpected proxy error: %v", err)
			}
		})
	}
}

func TestNeteaseHTTPClient(t *testing.T) {
	client, err := (NeteaseConfig{}).HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	if client.Transport != nil || client.Timeout != 0 {
		t.Fatal("empty proxy should preserve default transport and timeout")
	}
	if client, err := (NeteaseConfig{Proxy: "http://user:password@localhost:bad"}).HTTPClient(); err == nil || client != nil || strings.Contains(err.Error(), "password") {
		t.Fatal("parse errors should be returned without credentials")
	}

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.String() != "http://target.invalid/song?id=1" {
			t.Errorf("unexpected proxy target: %s", r.URL)
		}
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNzd29yZA==" {
			t.Error("missing proxy authentication")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()
	defaultTransport := http.DefaultTransport.(*http.Transport)
	defaultProxy := fmt.Sprintf("%p", defaultTransport.Proxy)
	cfg := Default()
	cfg.Netease.Proxy = strings.Replace(proxy.URL, "://", "://user:password@", 1)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	client, err = cfg.Netease.HTTPClient()
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == defaultTransport || client.Timeout != 0 {
		t.Fatal("explicit proxy should clone transport without adding a timeout")
	}
	if fmt.Sprintf("%p", defaultTransport.Proxy) != defaultProxy {
		t.Fatal("default transport proxy was modified")
	}
	resp, err := client.Get("http://target.invalid/song?id=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("proxy response status = %d", resp.StatusCode)
	}
}

func TestNeteaseHTTPClientSOCKS5(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			handshake := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					handshake <- err
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					handshake <- err
					return
				}
				var greeting [2]byte
				if _, err := io.ReadFull(conn, greeting[:]); err != nil {
					handshake <- err
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					handshake <- err
					return
				}
				if greeting[0] != 5 || len(methods) == 0 || methods[0] != 0 {
					handshake <- fmt.Errorf("unexpected SOCKS5 greeting: %v %v", greeting, methods)
					return
				}
				_, err = conn.Write([]byte{5, 255})
				handshake <- err
			}()
			cfg := Default()
			cfg.Netease.Proxy = scheme + "://" + listener.Addr().String()
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			client, err := cfg.Netease.HTTPClient()
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			client.Timeout = 5 * time.Second
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("failed SOCKS5 proxy must not fall back to a direct request")
			}))
			defer target.Close()
			if resp, err := client.Get(target.URL); err == nil {
				resp.Body.Close()
				t.Error("SOCKS5 rejection should fail the request")
			}
			listener.Close()
			select {
			case err := <-handshake:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SOCKS5 handshake was not received")
			}
		})
	}
}
