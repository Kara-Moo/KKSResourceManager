package main

// thumbs.go —— 缩略图队列的初始化与生成器
//
// 关键设计（红线 N1）：
//
//	UI 请求路径只查缓存，绝不触发 PNG 解码。
//	解码只发生在后台 worker 里，且并发 ≤ 3。
//
// 冷启动绝不预生成全部缩略图 —— 16150 张 × 30ms = 8 分钟。
//	只在用户真正聚焦到某个目录时，才按需生成那 9 张。

import (
	"encoding/base64"
	"os"
	"path/filepath"

	"kksresourcemanager/internal/preview"
)

// ThumbDirName 缩略图磁盘缓存目录名（位于 DELCARDS/_state 下）。
const ThumbDirName = "thumb"

// StartThumbQueue 启动缩略图队列。
//
// notify 用于批量通知前端（100ms 窗口），由调用方注入。
// onErr 接收单张生成失败的错误（可为 nil）。
//
// 幂等：已启动时直接返回 nil。这一点很重要 ——
// startup() 与 SelectKKSRoot() 都会调用它，后者才是真正的启动点
// （startup 时 manager 还是 nil，那是 v2.2 FIX-01 的成因）。
func (m *Manager) StartThumbQueue(notify preview.NotifyFunc, onErr func(path string, err error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.thumbs != nil {
		return nil
	}

	diskDir := filepath.Join(m.DelCardsRoot, "_state", ThumbDirName)
	if err := os.MkdirAll(diskDir, 0755); err != nil {
		return err
	}

	m.thumbs = preview.NewQueue(diskDir, 3, m.generateThumb, notify)
	m.thumbs.SetErrorHandler(onErr)
	return nil
}

// generateThumb 生成一张缩略图。
//
// 复用 preview.go 的解码/缩放逻辑，但输出 JPEG 字节而非 data URI
// （磁盘缓存存原始字节，命中时只需 base64 编码）。
func (m *Manager) generateThumb(abs string, fp preview.Fingerprint) (string, error) {
	data, err := m.buildThumbJPEG(abs)
	if err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data), nil
}

// StopThumbQueue 停止队列（应用退出时调用）。
func (m *Manager) StopThumbQueue() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.thumbs != nil {
		m.thumbs.Close()
		m.thumbs = nil
	}
}

// ThumbQueueRunning 队列是否已启动。
//
// 单独暴露而不只看 Stats：队列未启动时 Stats 全 0，
// 与「已启动但还没请求过」无法区分，而这两者的处置完全不同。
func (m *Manager) ThumbQueueRunning() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.thumbs != nil
}

// ThumbStats 返回缩略图队列统计（性能埋点与测试断言用）。
func (m *Manager) ThumbStats() preview.Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.thumbs == nil {
		return preview.Stats{}
	}
	return m.thumbs.Stats()
}
