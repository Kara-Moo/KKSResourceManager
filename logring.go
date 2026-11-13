package main

// logring.go —— 后端日志环形缓冲
//
// 重构规格 §10.3：
//
//	日志由后端环形缓冲提供（容量 5000 条），前端订阅增量事件，
//	而不是自己拼字符串。这样刷新/重开窗口不丢日志。
//
// §13.1 日志格式：
//
//	[<scope>] <动作> <对象> → <结果>

import (
	"sync"
	"time"
)

// LogEntry 一条日志。
type LogEntry struct {
	// Seq 全局递增序号，前端据此判断增量
	Seq   int64  `json:"seq"`
	TS    int64  `json:"ts"`
	Level string `json:"level"` // info / success / warn / error
	Scope string `json:"scope"` // scan / isolate / paste / plugin / index / ui
	Msg   string `json:"msg"`
}

// logRing 环形日志缓冲。
type logRing struct {
	mu      sync.RWMutex
	entries []LogEntry
	cap     int
	seq     int64
}

func newLogRing(capacity int) *logRing {
	if capacity <= 0 {
		capacity = 1000
	}
	return &logRing{
		entries: make([]LogEntry, 0, capacity),
		cap:     capacity,
	}
}

// Append 追加一条日志，返回其序号与条目。
func (r *logRing) Append(level, scope, msg string) LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	entry := LogEntry{
		Seq:   r.seq,
		TS:    time.Now().UnixMilli(),
		Level: level,
		Scope: scope,
		Msg:   msg,
	}
	r.entries = append(r.entries, entry)
	// 环形淘汰：超出容量时丢弃最旧的
	if len(r.entries) > r.cap {
		over := len(r.entries) - r.cap
		r.entries = append(r.entries[:0], r.entries[over:]...)
	}
	return entry
}

// Since 返回序号大于 sinceSeq 的日志条目。
func (r *logRing) Since(sinceSeq int64) []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]LogEntry, 0, 64)
	for _, e := range r.entries {
		if e.Seq > sinceSeq {
			result = append(result, e)
		}
	}
	return result
}

// All 返回全部日志。
func (r *logRing) All() []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]LogEntry, len(r.entries))
	copy(result, r.entries)
	return result
}

// Stats 返回统计信息。
func (r *logRing) Stats() (total int, errors int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	total = len(r.entries)
	for _, e := range r.entries {
		if e.Level == "error" {
			errors++
		}
	}
	return total, errors
}

// CurrentSeq 返回当前最大序号。
func (r *logRing) CurrentSeq() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.seq
}
