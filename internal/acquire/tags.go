package acquire

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	marker "github.com/XiaoMengXinX/163KeyMarker"
	"github.com/bogem/id3v2"
	"github.com/go-flac/flacpicture"
	"github.com/go-flac/flacvorbis"
	goflac "github.com/go-flac/go-flac"
	"github.com/nfnt/resize"
	_ "golang.org/x/image/webp"
)

// tagSet 是写进音频文件的元数据
type tagSet struct {
	Title       string
	Artists     []string
	Album       string
	AlbumArtist string
	Track       int
	Disc        int
	Year        int
	Key163      string // 网易云客户端识别用的 163 key
	Comment     string // 检测结论（有问题时带 ⚠）
	Quality     string // 机器可读的检测结论
}

func (t tagSet) artist() string { return strings.Join(t.Artists, ", ") }

func create163Key(m marker.MarkerData) string { return marker.Create163KeyStr(m) }

// writeTags 按文件类型写标签，覆盖已有标签和封面
func writeTags(path, format string, t tagSet, cover []byte, coverMIME string) error {
	switch format {
	case "flac":
		return writeFLACTags(path, t, cover, coverMIME)
	case "mp3":
		return writeMP3Tags(path, t, cover, coverMIME)
	}
	return fmt.Errorf("不支持给 %s 写标签", format)
}

func writeFLACTags(path string, t tagSet, cover []byte, coverMIME string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	file, err := goflac.ParseMetadata(in)
	if err != nil {
		return fmt.Errorf("读取 FLAC 元数据失败: %w", err)
	}
	var blocks []*goflac.MetaDataBlock
	for _, b := range file.Meta {
		switch b.Type {
		case goflac.VorbisComment, goflac.Picture, goflac.Padding:
			continue
		}
		blocks = append(blocks, b)
	}
	if len(blocks) == 0 || blocks[0].Type != goflac.StreamInfo {
		return errors.New("FLAC 缺少 STREAMINFO")
	}

	vc := flacvorbis.New()
	add := func(k, v string) {
		if strings.TrimSpace(v) != "" {
			_ = vc.Add(k, v)
		}
	}
	add(flacvorbis.FIELD_TITLE, t.Title)
	add(flacvorbis.FIELD_ARTIST, t.artist())
	add(flacvorbis.FIELD_ALBUM, t.Album)
	add("ALBUMARTIST", t.AlbumArtist)
	if t.Track > 0 {
		add(flacvorbis.FIELD_TRACKNUMBER, strconv.Itoa(t.Track))
	}
	if t.Disc > 0 {
		add("DISCNUMBER", strconv.Itoa(t.Disc))
	}
	if t.Year > 0 {
		add(flacvorbis.FIELD_DATE, strconv.Itoa(t.Year))
	}
	add(flacvorbis.FIELD_DESCRIPTION, t.Key163)
	add("COMMENT", t.Comment)
	add("QUALITY_CHECK", t.Quality)
	vcb := vc.Marshal()
	blocks = append(blocks, &vcb)
	if len(cover) > 0 {
		pic, err := flacpicture.NewFromImageData(flacpicture.PictureTypeFrontCover, "Front cover", cover, coverMIME)
		if err == nil {
			pb := pic.Marshal()
			blocks = append(blocks, &pb)
		}
	}
	blocks = append(blocks, &goflac.MetaDataBlock{Type: goflac.Padding, Data: make([]byte, 4096)})

	tmp := path + ".tag"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := out.Write([]byte("fLaC")); err != nil {
		return fail(err)
	}
	for i, b := range blocks {
		if _, err := out.Write(b.Marshal(i == len(blocks)-1)); err != nil {
			return fail(err)
		}
	}
	// ParseMetadata 恰好读到第一帧之前，剩下的音频帧原样复制
	if _, err := io.Copy(out, in); err != nil {
		return fail(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = in.Close()
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func writeMP3Tags(path string, t tagSet, cover []byte, coverMIME string) error {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: false})
	if err != nil {
		return err
	}
	defer tag.Close()
	tag.SetDefaultEncoding(id3v2.EncodingUTF8)
	tag.SetVersion(4)
	tag.SetTitle(t.Title)
	tag.SetArtist(t.artist())
	tag.SetAlbum(t.Album)
	if t.AlbumArtist != "" {
		tag.AddTextFrame("TPE2", id3v2.EncodingUTF8, t.AlbumArtist)
	}
	if t.Track > 0 {
		tag.AddTextFrame(tag.CommonID("Track number/Position in set"), id3v2.EncodingUTF8, strconv.Itoa(t.Track))
	}
	if t.Disc > 0 {
		tag.AddTextFrame(tag.CommonID("Part of a set"), id3v2.EncodingUTF8, strconv.Itoa(t.Disc))
	}
	if t.Year > 0 {
		tag.SetYear(strconv.Itoa(t.Year))
	}
	if t.Key163 != "" {
		tag.AddCommentFrame(id3v2.CommentFrame{Encoding: id3v2.EncodingISO, Language: "chs", Text: t.Key163})
	}
	if t.Comment != "" {
		tag.AddCommentFrame(id3v2.CommentFrame{Encoding: id3v2.EncodingUTF8, Language: "chi", Description: "quality", Text: t.Comment})
	}
	if len(cover) > 0 {
		tag.AddAttachedPicture(id3v2.PictureFrame{
			Encoding:    id3v2.EncodingUTF8,
			MimeType:    coverMIME,
			PictureType: id3v2.PTFrontCover,
			Description: "Front cover",
			Picture:     cover,
		})
	}
	return tag.Save()
}

// prepareCover 统一成 baseline JPEG（Walkman 不显示渐进式 JPEG 和 WebP），超过 maxPx 时等比缩小
func prepareCover(data []byte, maxPx int) ([]byte, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("无法识别封面图片: %w", err)
	}
	fits := maxPx == 0 || (cfg.Width <= maxPx && cfg.Height <= maxPx)
	if format == "jpeg" && fits && isBaselineJPEG(data) {
		return data, "image/jpeg", nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("封面解码失败: %w", err)
	}
	if !fits {
		w, h := uint(maxPx), uint(0)
		if cfg.Height > cfg.Width {
			w, h = 0, uint(maxPx)
		}
		img = resize.Resize(w, h, img, resize.Lanczos3)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

// isBaselineJPEG 扫描 JPEG 段标记，遇到 SOF2（渐进式）返回 false
func isBaselineJPEG(data []byte) bool {
	i := 2
	for i+4 <= len(data) {
		if data[i] != 0xFF {
			return false
		}
		m := data[i+1]
		switch {
		case m == 0xD8 || (m >= 0xD0 && m <= 0xD7) || m == 0x01:
			i += 2
			continue
		case m == 0xC0 || m == 0xC1:
			return true
		case m >= 0xC2 && m <= 0xCF && m != 0xC4 && m != 0xC8 && m != 0xCC:
			return false
		case m == 0xDA:
			return false
		}
		size := int(data[i+2])<<8 | int(data[i+3])
		i += 2 + size
	}
	return false
}

var lrcTime = regexp.MustCompile(`^\[(\d{1,3}):(\d{1,2})(?:[.:](\d{1,3}))?\]`)

// mergeLRC 在每行原文下面插入同一时间戳的译文
func mergeLRC(orig, trans string) string {
	key := func(line string) (string, bool) {
		m := lrcTime.FindStringSubmatch(line)
		if m == nil {
			return "", false
		}
		mm, _ := strconv.Atoi(m[1])
		ss, _ := strconv.Atoi(m[2])
		frac := m[3]
		for len(frac) < 3 {
			frac += "0"
		}
		ms, _ := strconv.Atoi(frac)
		return strconv.Itoa((mm*60+ss)*1000 + ms), true
	}
	tr := map[string]string{}
	for _, line := range strings.Split(trans, "\n") {
		line = strings.TrimRight(line, "\r")
		if k, ok := key(line); ok {
			if text := strings.TrimSpace(lrcTime.ReplaceAllString(line, "")); text != "" {
				tr[k] = line
			}
		}
	}
	var out []string
	for _, line := range strings.Split(orig, "\n") {
		line = strings.TrimRight(line, "\r")
		out = append(out, line)
		if k, ok := key(line); ok {
			if t, ok := tr[k]; ok {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n")
}

func hasLRCTimestamps(lrc string) bool {
	for _, line := range strings.Split(lrc, "\n") {
		if lrcTime.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}
