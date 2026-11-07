// Package logging 配置日志格式与按天切分的日志文件
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

type Formatter struct{}

func (f *Formatter) Format(entry *logrus.Entry) ([]byte, error) {
	ts := time.Now().Local().Format("2006/01/02 15:04:05")
	return []byte(fmt.Sprintf("%s [%s] %s (%s:%d)\n",
		ts, strings.ToUpper(entry.Level.String()), entry.Message,
		filepath.Base(entry.Caller.File), entry.Caller.Line)), nil
}

type dailyFileWriter struct {
	mu     sync.Mutex
	dir    string
	day    string
	f      *os.File
	closed bool
}

func (w *dailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return len(p), nil
	}
	day := time.Now().Local().Format("2006-01-02")
	if w.f == nil || day != w.day {
		if err := os.MkdirAll(w.dir, 0o755); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(filepath.Join(w.dir, day+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return 0, err
		}
		if w.f != nil {
			_ = w.f.Close()
		}
		w.f, w.day = f, day
	}
	return w.f.Write(p)
}

func (w *dailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Setup 把日志输出到 os.Stdout（main 启动时已把它换成 stderr），dir 非空时同时写到 dir/日期.log。
// 返回的 close 关闭日志文件，之后的日志只输出到终端
func Setup(level, dir string) (*logrus.Logger, func() error) {
	l := logrus.New()
	l.SetFormatter(&Formatter{})
	l.SetReportCaller(true)
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		lvl = logrus.InfoLevel
	}
	l.SetLevel(lvl)
	if dir == "" {
		l.SetOutput(os.Stdout)
		return l, func() error { return nil }
	}
	file := &dailyFileWriter{dir: dir}
	l.SetOutput(io.MultiWriter(os.Stdout, file))
	return l, file.Close
}
