package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/hoganyugit/music163-mcp/internal/acquire"
)

const jobRetention = 2 * time.Hour

// job 是一次在后台运行的下载，调用方断开或超时后仍会继续
type job struct {
	id      string
	musicID int
	level   string
	started time.Time

	mu        sync.Mutex
	stage     string
	done      int64
	total     int64
	seq       float64
	finished  time.Time
	result    *acquire.Result
	err       error
	listeners map[int]func(p progressEvent)
	nextL     int
	doneCh    chan struct{}
}

type progressEvent struct {
	seq   float64
	stage string
	done  int64
	total int64
}

func (j *job) onProgress(stage string, done, total int64) {
	j.mu.Lock()
	j.stage, j.done, j.total = stage, done, total
	j.seq++
	ev := progressEvent{seq: j.seq, stage: stage, done: done, total: total}
	ls := make([]func(progressEvent), 0, len(j.listeners))
	for _, l := range j.listeners {
		ls = append(ls, l)
	}
	j.mu.Unlock()
	for _, l := range ls {
		l(ev)
	}
}

func (j *job) subscribe(fn func(progressEvent)) func() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.listeners == nil {
		j.listeners = map[int]func(progressEvent){}
	}
	id := j.nextL
	j.nextL++
	j.listeners[id] = fn
	return func() {
		j.mu.Lock()
		delete(j.listeners, id)
		j.mu.Unlock()
	}
}

type jobSnapshot struct {
	stage    string
	done     int64
	total    int64
	finished bool
	result   *acquire.Result
	err      error
}

func (j *job) snapshot() jobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return jobSnapshot{stage: j.stage, done: j.done, total: j.total, finished: !j.finished.IsZero(), result: j.result, err: j.err}
}

// wait 等到任务结束、超时或请求被取消
func (j *job) wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-j.doneCh:
	case <-t.C:
	case <-ctx.Done():
	}
}

type jobs struct {
	running sync.WaitGroup

	mu     sync.Mutex
	byID   map[string]*job
	bySong map[int]*job
}

func newJobs() *jobs {
	return &jobs{byID: map[string]*job{}, bySong: map[int]*job{}}
}

func newJobID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "dl-" + hex.EncodeToString(b)
}

// start 为一首歌启动下载；同一首歌已有任务在跑时返回那个任务
func (js *jobs) start(ctx context.Context, musicID int, level string, run func(ctx context.Context, progress acquire.ProgressFunc) (*acquire.Result, error)) (*job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.gcLocked()
	if j, ok := js.bySong[musicID]; ok {
		return j, false
	}
	j := &job{id: newJobID(), musicID: musicID, level: level, started: time.Now(), stage: "准备中", doneCh: make(chan struct{})}
	js.byID[j.id] = j
	js.bySong[musicID] = j
	js.running.Add(1)
	go func() {
		defer js.running.Done()
		res, err := run(ctx, j.onProgress)
		j.mu.Lock()
		j.result, j.err, j.finished = res, err, time.Now()
		j.mu.Unlock()
		js.mu.Lock()
		if js.bySong[musicID] == j {
			delete(js.bySong, musicID)
		}
		js.mu.Unlock()
		close(j.doneCh)
	}()
	return j, true
}

// drain 等所有任务结束，最多等 d
func (js *jobs) drain(d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		js.running.Wait()
		close(done)
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func (js *jobs) get(id string) (*job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.byID[id]
	return j, ok
}

func (js *jobs) gcLocked() {
	for id, j := range js.byID {
		j.mu.Lock()
		old := !j.finished.IsZero() && time.Since(j.finished) > jobRetention
		j.mu.Unlock()
		if old {
			delete(js.byID, id)
		}
	}
}
