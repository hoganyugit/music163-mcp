package textx

import "testing"

func TestParseMusicID(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"完整歌曲链接", "https://music.163.com/song?id=1824020873&userid=1", 1824020873},
		{"路径式歌曲链接", "https://music.163.com/song/1824020873/", 1824020873},
		{"手机端链接（id 不在首位）", "https://y.music.163.com/m/song?app_version=9.5.05&id=1824020873&userid=1", 1824020873},
		{"纯数字", "1824020873", 1824020873},
		{"分享文本带链接", "校正中分享了一首歌曲 https://music.163.com/song?id=1824020873", 1824020873},
		{"短链文本", "https://163cn.tv/AbCdEf", 0},
		{"专辑链接不算歌曲", "https://music.163.com/album?id=1", 0},
		{"空文本", "", 0},
		{"无数字文本", "hello world", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseMusicID(tt.text); got != tt.want {
				t.Errorf("ParseMusicID(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestURLBase(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://m801.music.126.net/abc/def.mp3?auth=1&x=2", "def.mp3"},
		{"https://p1.music.126.net/xxx/cover.jpg", "cover.jpg"},
		{"https://p1.music.126.net/xxx/cover.jpg#frag", "cover.jpg"},
	}
	for _, tt := range tests {
		if got := URLBase(tt.url); got != tt.want {
			t.Errorf("URLBase(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestContainsMusicLink(t *testing.T) {
	if !ContainsMusicLink("看这个 https://music.163.com/song?id=1") {
		t.Error("music.163.com 应命中")
	}
	if !ContainsMusicLink("https://163cn.tv/xyz") {
		t.Error("163cn.tv 应命中")
	}
	if !ContainsMusicLink("https://163cn.link/xyz") {
		t.Error("163cn.link 应命中")
	}
	if ContainsMusicLink("普通消息") {
		t.Error("普通消息不应命中")
	}
}

func TestContainsShortLink(t *testing.T) {
	if !ContainsShortLink("https://163cn.tv/xyz") || !ContainsShortLink("https://163cn.link/xyz") {
		t.Error("163cn 短链应命中")
	}
	if ContainsShortLink("https://music.163.com/song?id=1") {
		t.Error("完整链接不是短链")
	}
}

func TestResolveShortLink(t *testing.T) {
	if got := ResolveShortLink("https://music.163.com/song?id=1"); got != "https://music.163.com/song?id=1" {
		t.Errorf("非短链应原样返回, got %q", got)
	}
	if got := ResolveShortLink("see https://example.com/163cn.tv/foo"); got != "see https://example.com/163cn.tv/foo" {
		t.Errorf("非 163cn 短链应原样返回, got %q", got)
	}
}

// 无法被 url.Parse 解析的链接不能 panic
func TestParseMusicIDMalformedURL(t *testing.T) {
	for _, in := range []string{
		"https://music.163.com/song%",
		"https://music.163.com:abc/song?id=1",
	} {
		_ = ParseMusicID(in)
	}
	if got := ParseMusicID("https://music.163.com:abc/song?id=123"); got != 123 {
		t.Fatalf("ParseMusicID fallback = %d, want 123", got)
	}
}
