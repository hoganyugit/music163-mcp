// Package netease 网易云 API 封装（Music163Api-Go）
package netease

import (
	"net/http"
	"sync"

	"github.com/XiaoMengXinX/Music163Api-Go/api"
	"github.com/XiaoMengXinX/Music163Api-Go/types"
	"github.com/XiaoMengXinX/Music163Api-Go/utils"
)

type Client struct {
	mu   sync.RWMutex
	data utils.RequestData
	http *http.Client
	gate *Gate // 为 nil 时不限速
}

// musicU 为空时匿名请求
func New(musicU string, client *http.Client) *Client {
	c := &Client{http: client}
	if musicU != "" {
		c.data = utils.RequestData{
			Cookies: []*http.Cookie{
				{Name: "MUSIC_U", Value: musicU},
			},
		}
	}
	return c
}

func (c *Client) SearchSong(keyword string, limit, offset int) (types.SearchSongData, error) {
	return api.SearchSong(c.requestData(), api.SearchSongConfig{
		Keyword: keyword,
		Limit:   limit,
		Offset:  offset,
	})
}
