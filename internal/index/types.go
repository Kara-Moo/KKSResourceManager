// Package index 实现 KKS 资源索引的持久化与增量维护。
//
// 设计要点（v2.4 阶段 3 / 规格 §5）：
//
//   - **只持久化正向数据**（Cards：指纹 + 类型 + 相对路径）。
//     不再保存 zipmod / plugin / 依赖图 —— 本工具已不管理 zipmod。
//
//   - **索引文件放在 DELCARDS/_state/index.json**，
//     绝不放进 KKS_ROOT（红线 R2：游戏目录只被「移动资源」改变）。
//
//   - **原子写入**：写 .tmp → Sync → Rename。
//     避免进程被杀导致索引文件半截损坏。
//
//   - **SchemaVersion 2**：读到 v1 旧索引时丢弃内存数据、全量重建；
//     旧文件只被后续 Save 覆盖写，绝不主动删除。
package index

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion 索引结构版本。
//
// v1 → v2：删除 zipmod / plugin / 依赖图，只保留卡片指纹。
// 变更此值会导致旧索引被丢弃并全量重建。
const SchemaVersion = 2

// Fingerprint 文件指纹，用于判断文件是否变化。
type Fingerprint struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"` // UnixNano
}

// Equal 判断两个指纹是否相同。
func (f Fingerprint) Equal(o Fingerprint) bool {
	return f.Size == o.Size && f.ModTime == o.ModTime
}

// FingerprintOf 从 stat 结果构造指纹。
func FingerprintOf(size int64, modTime time.Time) Fingerprint {
	return Fingerprint{Size: size, ModTime: modTime.UnixNano()}
}

// CardEntry 一个资源卡片的索引条目。
type CardEntry struct {
	Fingerprint
	// Kind 资源类型（female_card / scene / ...）
	Kind string `json:"kind"`
	// Rel 相对该类型资源根的路径（POSIX 分隔符）
	Rel string `json:"rel"`
}

// cardEntryWire 是 CardEntry 的紧凑持久化形式。
//
// 默认结构体编码每条约 164 字节（"size"/"mtime"/"kind"/"rel" 字段名 +
// 两个长整数），16150 张卡会突破 index.json ≤2MB 的硬指标。
// 指纹压成 "size:mtime"、字段名用单字母后，实测 12973 张卡约 1.87MiB。
// 这只是磁盘格式，Go 侧仍使用语义清晰的 CardEntry.Fingerprint/Kind/Rel。
type cardEntryWire struct {
	FP   string `json:"fp"` // "size:mtime"
	Kind string `json:"k"`
	Rel  string `json:"r"`
}

// MarshalJSON 实现紧凑序列化。
func (e CardEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(cardEntryWire{
		FP:   strconv.FormatInt(e.Size, 10) + ":" + strconv.FormatInt(e.ModTime, 10),
		Kind: e.Kind,
		Rel:  e.Rel,
	})
}

// UnmarshalJSON 同时接受紧凑形式与早期对象形式
// （{"size":..,"mtime":..,"kind":..,"rel":..}）。
//
// 兼容旧形式的意义：阶段 3 尚未发布，但如果磁盘上已有上一版 v2 索引，
// 不应因为编码变化整份作废、被迫全量重建。
func (e *CardEntry) UnmarshalJSON(data []byte) error {
	var v struct {
		FP   string `json:"fp"`
		Kind string `json:"k"`
		Rel  string `json:"r"`
		// 早期 v2 形式
		Size       int64  `json:"size"`
		ModTime    int64  `json:"mtime"`
		LegacyKind string `json:"kind"`
		LegacyRel  string `json:"rel"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	if v.FP != "" {
		sizeStr, timeStr, ok := strings.Cut(v.FP, ":")
		if !ok {
			return fmt.Errorf("无效的卡片指纹: %q", v.FP)
		}
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的卡片指纹大小 %q: %w", sizeStr, err)
		}
		mtime, err := strconv.ParseInt(timeStr, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的卡片指纹时间 %q: %w", timeStr, err)
		}
		e.Fingerprint = Fingerprint{Size: size, ModTime: mtime}
		e.Kind = v.Kind
		e.Rel = v.Rel
		return nil
	}

	e.Fingerprint = Fingerprint{Size: v.Size, ModTime: v.ModTime}
	e.Kind = v.LegacyKind
	e.Rel = v.LegacyRel
	return nil
}

// IndexData 索引的持久化内容。
type IndexData struct {
	SchemaVersion int   `json:"schemaVersion"`
	Root          string `json:"root"` // 绝对路径；与当前 root 不一致则全量重建
	Cards         map[string]*CardEntry `json:"cards"` // key = 绝对路径
	ScannedAt     int64 `json:"scannedAt"`
}

// NewIndexData 创建一个空索引。
func NewIndexData(root string) *IndexData {
	return &IndexData{
		SchemaVersion: SchemaVersion,
		Root:          root,
		Cards:         make(map[string]*CardEntry),
		ScannedAt:     time.Now().UnixNano(),
	}
}

// IsCompatible 判断旧索引能否复用。
//
// 条件：版本一致 + root 一致 + Cards 非 nil。
// 任一不满足则返回 false，调用方应全量重建。
func (d *IndexData) IsCompatible(root string) bool {
	if d == nil {
		return false
	}
	if d.SchemaVersion != SchemaVersion {
		return false
	}
	if d.Root != root {
		return false
	}
	if d.Cards == nil {
		return false
	}
	return true
}

// EnsureMaps 补齐可能为 nil 的 map，避免后续写入 panic。
func (d *IndexData) EnsureMaps() {
	if d.Cards == nil {
		d.Cards = make(map[string]*CardEntry)
	}
}
