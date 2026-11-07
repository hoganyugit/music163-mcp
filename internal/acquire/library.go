package acquire

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hoganyugit/music163-mcp/internal/netease"
)

// FAT/exFAT（Walkman 的 microSD）不允许的字符换成全角，保留可读性
var nameReplacer = strings.NewReplacer(
	"/", "／", "\\", "＼", ":", "：", "*", "＊", "?", "？", "\"", "＂", "<", "＜", ">", "＞", "|", "｜",
)

const maxNameBytes = 180 // 给扩展名、"-id3v2" 临时后缀和曲号留余量，远低于 255 字节限制

// safeName 生成在 ext4/exFAT/NTFS 上都合法的文件名或目录名
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = nameReplacer.Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, ". ")
	if len(s) > maxNameBytes {
		n := maxNameBytes
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = strings.TrimRight(s[:n], ". ")
	}
	if s == "" {
		return "_"
	}
	return s
}

func discNumber(disc string) int {
	disc = strings.TrimSpace(disc)
	if i := strings.IndexAny(disc, "/-"); i >= 0 {
		disc = disc[:i]
	}
	n, _ := strconv.Atoi(disc)
	return n
}

// libraryPath 返回 曲库/歌手/专辑/[碟号-]曲号 歌名.ext
func libraryPath(root string, m netease.SongMeta, ext string) string {
	artist := "未知歌手"
	if len(m.Artists) > 0 && strings.TrimSpace(m.Artists[0].Name) != "" {
		artist = m.Artists[0].Name
	}
	album := m.Album
	if strings.TrimSpace(album) == "" {
		album = "未知专辑"
	}
	base := m.Name
	if m.TrackNo > 0 {
		prefix := fmt.Sprintf("%02d ", m.TrackNo)
		if d := discNumber(m.Disc); d > 1 {
			prefix = fmt.Sprintf("%d-%02d ", d, m.TrackNo)
		}
		base = prefix + base
	}
	return filepath.Join(root, safeName(artist), safeName(album), safeName(base)+"."+ext)
}

// moveFile 先尝试 rename，跨文件系统（如曲库在 microSD/NAS 上）时复制后删除源文件
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	tmp := dst + ".part"
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = in.Close()
	return os.Remove(src)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Service) reportPath(musicID int) string {
	return filepath.Join(s.lib.StateDir, "reports", strconv.Itoa(musicID)+".json")
}

func (s *Service) spectrogramPath(musicID int, level string) string {
	return filepath.Join(s.lib.StateDir, "spectrograms", fmt.Sprintf("%d-%s.png", musicID, level))
}

// LoadResult 读取某首歌上次的下载结果
func (s *Service) LoadResult(musicID int) (*Result, error) {
	data, err := os.ReadFile(s.reportPath(musicID))
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Service) saveResult(r *Result) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.reportPath(r.MusicID), data, 0o644)
}

// existing 返回上次下载且文件仍在的结果
func (s *Service) existing(musicID int) (*Result, bool) {
	r, err := s.LoadResult(musicID)
	if err != nil || r.Path == "" {
		return nil, false
	}
	if st, err := os.Stat(r.Path); err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	r.AlreadyExisted = true
	return r, true
}

// claimPath 解决重名：同一首歌的旧文件直接覆盖，别的歌占用了就在文件名后加 [ID]
func (s *Service) claimPath(musicID int, path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	if prev, err := s.LoadResult(musicID); err == nil && prev.Path == path {
		return path
	}
	ext := filepath.Ext(path)
	return strings.TrimSuffix(path, ext) + fmt.Sprintf(" [%d]", musicID) + ext
}
