package netease

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/XiaoMengXinX/Music163Api-Go/types"
)

func urlData(t *testing.T, raw string) types.SongsURLData {
	t.Helper()
	var d types.SongsURLData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	d.RawJson = raw
	return d
}

func TestParseSongURLDowngradeAndTrial(t *testing.T) {
	raw := `{"code":200,"data":[{"id":1,"url":"http://m701.music.126.net/a.flac","br":1702000,"size":51234567,"md5":"ABCDEF","code":200,"type":"FLAC","fee":8,"level":"lossless","time":242000,"freeTrialInfo":null}]}`
	u, err := parseSongURL(urlData(t, raw), LevelJymaster)
	if err != nil {
		t.Fatal(err)
	}
	if u.Level != LevelLossless || u.Requested != LevelJymaster || u.Type != "flac" || u.MD5 != "abcdef" || u.Trial || !u.Available() {
		t.Fatalf("unexpected %+v", u)
	}

	raw = `{"code":200,"data":[{"id":1,"url":"http://m701.music.126.net/b.mp3","br":128000,"size":480000,"md5":"x","code":200,"type":"mp3","fee":1,"level":"standard","freeTrialInfo":{"start":12,"end":42}}]}`
	u, err = parseSongURL(urlData(t, raw), LevelHires)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Trial || u.TrialFrom != 12 || u.TrialTo != 42 {
		t.Fatalf("trial not detected: %+v", u)
	}

	raw = `{"code":200,"data":[{"id":1,"url":null,"br":0,"size":0,"md5":null,"code":404,"type":null,"fee":0,"level":null,"freeTrialInfo":null}]}`
	u, err = parseSongURL(urlData(t, raw), LevelHires)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available() || u.Code != 404 {
		t.Fatalf("unexpected %+v", u)
	}
}

func TestParseSongMetas(t *testing.T) {
	raw := `{"code":200,"songs":[{"name":"晴天","id":186016,"ar":[{"id":6452,"name":"周杰伦"}],"alia":["Sunny Day",1],"al":{"id":18905,"name":"叶惠美","picUrl":"http://p1.music.126.net/x.jpg"},"dt":269000,"no":3,"cd":"01","publishTime":1059580800000,"fee":1,"hr":null,"sq":{"br":918000,"size":30902000,"sr":44100},"h":{"br":320000,"size":10763000,"sr":44100}}],
	"privileges":[{"id":186016,"st":-100,"maxBrLevel":"vivid","playMaxBrLevel":"vivid"}]}`
	var d types.SongsDetailData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	d.RawJson = raw
	metas, err := parseSongMetas(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("len = %d", len(metas))
	}
	m := metas[0]
	if !m.Unavailable() || m.StereoMax() != LevelLossless {
		t.Fatalf("status/stereo max: %+v", m)
	}
	if m.Name != "晴天" || m.Album != "叶惠美" || m.TrackNo != 3 || m.Disc != "01" || m.MaxLevel != "vivid" ||
		len(m.Alias) != 1 || m.Sq == nil || m.Sq.SampleRate != 44100 || m.Hr != nil || m.Year() != 2003 || m.Detail.Id != 186016 {
		t.Fatalf("unexpected %+v", m)
	}
}

func TestParseVip(t *testing.T) {
	now := time.UnixMilli(1762400000000)
	raw := `{"code":200,"data":{"redVipLevel":7,"musicPackage":{"vipCode":220,"expireTime":1700000000000},"associator":{"vipCode":100,"expireTime":1790000000000},"redplus":{"vipCode":300,"expireTime":1790000000000},"userId":1}}`
	items, err := parseVip(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.Name] = it.Active
	}
	if !got["黑胶SVIP"] || !got["黑胶VIP"] || got["音乐包"] || len(items) != 3 {
		t.Fatalf("unexpected %+v", items)
	}
}

func TestLevelRank(t *testing.T) {
	if !(LevelRank(LevelJymaster) > LevelRank(LevelHires) && LevelRank(LevelHires) > LevelRank(LevelLossless) && LevelRank(LevelLossless) > LevelRank(LevelExhigh)) {
		t.Fatal("rank order wrong")
	}
	if LevelRank(LevelDolby) != 0 || !ValidLevel(LevelDolby) || !ValidLevel(LevelVivid) || ValidLevel("") || ValidLevel("ultra") {
		t.Fatal("spatial/invalid level handling wrong")
	}
}

func TestStereoMaxFallsBackToLowTiers(t *testing.T) {
	m := SongMeta{MaxLevel: LevelVivid, L: &FileSpec{Bitrate: 128000}}
	if got := m.StereoMax(); got != LevelStandard {
		t.Fatalf("StereoMax = %q", got)
	}
}
