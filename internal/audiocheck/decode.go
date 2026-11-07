package audiocheck

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hajimehoshi/go-mp3"
	"github.com/mewkiz/flac"
)

// decoded 是一次完整解码的结果
type decoded struct {
	format    string
	rate      int
	channels  int
	bits      int
	declared  int64  // 文件头声明的每声道采样数，未知为 0
	samples   int64  // 实际解码出的每声道采样数
	decodeErr string // 解码中途出错（CRC 错误、截断等）
	md5Status string // ok / mismatch / absent / n/a
	an        *analyzer
}

// sniff 按文件头识别容器格式（跳过 ID3v2 标签）
func sniff(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 12)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if bytes.HasPrefix(head, []byte("ID3")) && len(head) >= 10 {
		size := int64(head[6]&0x7f)<<21 | int64(head[7]&0x7f)<<14 | int64(head[8]&0x7f)<<7 | int64(head[9]&0x7f)
		off := 10 + size
		if head[5]&0x10 != 0 {
			off += 10
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "unknown", nil
		}
		n, _ = io.ReadFull(f, head[:4])
		head = head[:n]
		if bytes.HasPrefix(head, []byte("fLaC")) {
			return "flac", nil
		}
		return "mp3", nil
	}
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")):
		return "flac", nil
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return "mp3", nil
	case len(head) >= 8 && string(head[4:8]) == "ftyp":
		return "mp4", nil
	case bytes.HasPrefix(head, []byte("OggS")):
		return "ogg", nil
	case bytes.HasPrefix(head, []byte("RIFF")):
		return "wav", nil
	}
	return "unknown", nil
}

func decodeFLAC(path string) (*decoded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stream, err := flac.New(f)
	if err != nil {
		return nil, fmt.Errorf("无法解析 FLAC 文件头: %w", err)
	}
	info := stream.Info
	if info.SampleRate == 0 || info.NChannels == 0 || info.BitsPerSample < 8 || info.BitsPerSample > 24 {
		return nil, fmt.Errorf("不支持的 FLAC 参数: %d Hz, %d 声道, %d bit", info.SampleRate, info.NChannels, info.BitsPerSample)
	}
	d := &decoded{
		format:   "flac",
		rate:     int(info.SampleRate),
		channels: int(info.NChannels),
		bits:     int(info.BitsPerSample),
		declared: int64(info.NSamples),
	}
	d.an = newAnalyzer(d.rate, d.channels, d.bits, d.declared)
	h := md5.New()
	var buf []byte
	block := make([][]int32, d.channels)
	for {
		fr, err := stream.ParseNext()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			d.decodeErr = err.Error()
			break
		}
		if len(fr.Subframes) != d.channels || int(fr.BitsPerSample) != d.bits {
			d.decodeErr = fmt.Sprintf("第 %d 帧的声道数/位深与文件头不一致", fr.Num)
			break
		}
		for c, sf := range fr.Subframes {
			block[c] = sf.Samples
		}
		buf = appendPCM(buf[:0], block, d.bits)
		h.Write(buf)
		d.an.feed(block)
		d.samples += int64(len(block[0]))
	}
	switch {
	case info.MD5sum == [16]byte{}:
		d.md5Status = "absent"
	case d.decodeErr != "":
		d.md5Status = "n/a"
	case bytes.Equal(h.Sum(nil), info.MD5sum[:]):
		d.md5Status = "ok"
	default:
		d.md5Status = "mismatch"
	}
	return d, nil
}

// appendPCM 按 FLAC MD5 的约定（小端、交错、最少字节数）序列化采样
func appendPCM(buf []byte, block [][]int32, bits int) []byte {
	bps := (bits + 7) / 8
	n := len(block[0])
	need := n * len(block) * bps
	if cap(buf) < need {
		buf = make([]byte, need)
	}
	buf = buf[:need]
	i := 0
	for s := 0; s < n; s++ {
		for c := range block {
			v := block[c][s]
			switch bps {
			case 1:
				buf[i] = byte(v)
			case 2:
				buf[i], buf[i+1] = byte(v), byte(v>>8)
			default:
				buf[i], buf[i+1], buf[i+2] = byte(v), byte(v>>8), byte(v>>16)
			}
			i += bps
		}
	}
	return buf
}

func decodeMP3(path string) (*decoded, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec, err := mp3.NewDecoder(f)
	if err != nil {
		return nil, fmt.Errorf("无法解析 MP3: %w", err)
	}
	var total int64
	if l := dec.Length(); l > 0 {
		total = l / 4
	}
	d := &decoded{format: "mp3", rate: dec.SampleRate(), channels: 2, bits: 16, declared: total, md5Status: "n/a"}
	d.an = newAnalyzer(d.rate, 2, 16, total)
	const chunk = 4096
	buf := make([]byte, 4*chunk)
	l, r := make([]int32, chunk), make([]int32, chunk)
	for {
		n, err := io.ReadFull(dec, buf)
		k := n / 4
		for i := 0; i < k; i++ {
			l[i] = int32(int16(binary.LittleEndian.Uint16(buf[4*i:])))
			r[i] = int32(int16(binary.LittleEndian.Uint16(buf[4*i+2:])))
		}
		if k > 0 {
			d.an.feed([][]int32{l[:k], r[:k]})
			d.samples += int64(k)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			d.decodeErr = err.Error()
			break
		}
	}
	return d, nil
}
