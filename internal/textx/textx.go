// Package textx 歌曲链接/ID 解析与 163cn 短链跟随
package textx

import (
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	reg1   = regexp.MustCompile(`(.*)song\?id=`)
	reg2   = regexp.MustCompile(`(.*)song/`)
	reg3   = regexp.MustCompile(`\?(.*)`)
	reg4   = regexp.MustCompile("&(.*)")
	reg5   = regexp.MustCompile("/(.*)")
	regInt = regexp.MustCompile(`\d+`)
	regURL = regexp.MustCompile("(http|https)://[\\w\\-_]+(\\.[\\w\\-_]+)+([\\w\\-.,@?^=%&:/~+#]*[\\w\\-@?^=%&/~+#])?")
)

// URLBase 返回 URL 路径的最后一段，去掉 query 和 fragment
func URLBase(rawURL string) string {
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rawURL = rawURL[:i]
	}
	if i := strings.IndexByte(rawURL, '#'); i >= 0 {
		rawURL = rawURL[:i]
	}
	return path.Base(rawURL)
}

// ParseMusicID 从歌曲链接、分享文本或纯数字中取出歌曲 ID，取不到时返回 0
func ParseMusicID(text string) int {
	messageText := strings.NewReplacer("\n", "", " ", "").Replace(text)
	if musicURL := regURL.FindStringSubmatch(messageText); len(musicURL) != 0 {
		// url.Parse 失败时退回到正则提取
		if strings.Contains(musicURL[0], "song") {
			if ur, err := url.Parse(musicURL[0]); err == nil {
				if musicID, _ := strconv.Atoi(ur.Query().Get("id")); musicID != 0 {
					return musicID
				}
			}
		}
	}
	musicID, _ := strconv.Atoi(linkTestMusic(messageText))
	return musicID
}

func extractInt(text string) string {
	matchArr := regInt.FindStringSubmatch(text)
	if len(matchArr) == 0 {
		return ""
	}
	return matchArr[0]
}

func linkTestMusic(text string) string {
	return extractInt(reg5.ReplaceAllString(reg4.ReplaceAllString(reg3.ReplaceAllString(reg2.ReplaceAllString(reg1.ReplaceAllString(text, ""), ""), ""), ""), ""))
}

var shortLinkClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func ContainsMusicLink(text string) bool {
	return strings.Contains(text, "music.163.com") ||
		strings.Contains(text, "163cn.tv") ||
		strings.Contains(text, "163cn.link")
}

func ContainsShortLink(text string) bool {
	return strings.Contains(text, "163cn.tv") || strings.Contains(text, "163cn.link")
}

// ResolveShortLink 解析 163cn 短链返回重定向地址，无短链或失败时原样返回（校验 host）
func ResolveShortLink(text string) string {
	messageText := strings.NewReplacer("\n", "", " ", "").Replace(text)
	musicURL := regURL.FindStringSubmatch(messageText)
	if len(musicURL) == 0 {
		return text
	}
	u, err := url.Parse(musicURL[0])
	if err != nil || (u.Host != "163cn.tv" && u.Host != "163cn.link") {
		return text
	}
	resp, err := shortLinkClient.Get(musicURL[0])
	if err != nil {
		return text
	}
	defer resp.Body.Close()
	if location := resp.Header.Get("Location"); location != "" {
		return location
	}
	return text
}
