// Package index 实现 KKS 资源索引的持久化与增量维护。
//
// 设计要点（对应重构规格 §5.2）：
//
//   - **只持久化正向数据**（Zipmods / Cards / Plugins）。
//     GlobalDepMap、GuidToZipmod、MissingGUIDs 等反向索引一律不落盘，
//     启动时由正向数据现算（derive）。
//     理由：落盘就要双写，双写就会不一致，
//     不一致就会出现「明明还在用却提示可隔离」这种致命错误。
//
//   - **索引文件放在 DELCARDS/_state/index.json**，
//     绝不放进 KKS_ROOT（红线 R2：游戏目录只被「移动资源」改变）。
//
//   - **原子写入**：写 .tmp → Sync → Rename。
//     避免进程被杀导致索引文件半截损坏。
package index

import (
	"time"
)

// SchemaVersion 索引结构版本。
//
// 变更此值会导致旧索引被丢弃并全量重建。
const SchemaVersion = 1

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

// ZipmodEntry 一个 zipmod 的索引条目。
type ZipmodEntry struct {
	Fingerprint
	// GUID 该 zipmod 的 GUID（来自 manifest.xml）
	GUID string `json:"guid"`
	// Deps 该 zipmod 通过 manifest 声明的依赖 GUID
	Deps []string `json:"deps"`
	// Rel 它在 mods 下的相对路径（POSIX）。
	// 隔离后仍保留，用于修复缺失时放回原位。
	Rel string `json:"rel,omitempty"`
}

// CardEntry 一个资源卡片（PNG）的索引条目。
type CardEntry struct {
	Fingerprint
	// Kind 资源类型（female_card / scene / ...）
	Kind string `json:"kind"`
	// Rel 相对该类型资源根的路径（POSIX 分隔符）
	Rel string `json:"rel"`
	// GUIDs 解析出的依赖 GUID 列表。
	// 解析失败时为空切片（非 nil），便于区分「未解析」与「无依赖」。
	GUIDs []string `json:"guids"`
	// ParseErr 解析失败时的原因（成功时为空）
	ParseErr string `json:"parseErr,omitempty"`
}

// PluginEntry 一个 BepInEx 插件（DLL）的索引条目。
type PluginEntry struct {
	Fingerprint
	// Rel 相对 KKS_ROOT 的路径（POSIX 分隔符）
	Rel string `json:"rel"`
	// GUIDs 该 DLL 中硬编码引用到的 zipmod GUID
	GUIDs []string `json:"guids"`
}

// IndexData 索引的持久化内容。
//
// 只包含正向数据。派生索引由 Derive 计算，不落盘。
type IndexData struct {
	SchemaVersion int   `json:"schemaVersion"`
	Root          string `json:"root"`  // 绝对路径；与当前 root 不一致则全量重建
	ScannedAt     int64  `json:"scannedAt"`
	GuidSetHash   string `json:"guidSetHash"` // zipmod GUID 集合的哈希，用于判断 DLL 是否需重扫

	// Zipmods key = 绝对路径
	Zipmods map[string]*ZipmodEntry `json:"zipmods"`
	// Cards key = 绝对路径
	Cards map[string]*CardEntry `json:"cards"`
	// Plugins key = 绝对路径
	Plugins map[string]*PluginEntry `json:"plugins"`

	// IsolatedZipmods 被隔离到 DEL-zipmod 或 DELED 的 zipmod。
	//
	// key = 绝对路径，Rel 存原相对路径（mods 下的位置）。
	// **惰性扫描**：只在需要时填充（进入 zipmod 模式或执行修复时）。
	IsolatedZipmods map[string]*ZipmodEntry `json:"isolatedZipmods,omitempty"`

	// OriginRel GUID -> 它在 mods 下的原相对路径。
	//
	// 隔离时写入；修复缺失时据此把文件放回原位，未知则放 mods 根。
	OriginRel map[string]string `json:"originRel,omitempty"`

	// PluginDepMap GUID -> 引用它的 DLL 相对路径列表
	//
	// 这份数据是「DLL 反查 zipmod」的唯一依据。
	// 它由 Plugins 派生，但因体量小且查询频繁，单独保留一份，
	// 避免每次判定都遍历全部 DLL。
	PluginDepMap map[string][]string `json:"pluginDepMap"`
}

// NewIndexData 创建一个空索引。
func NewIndexData(root string) *IndexData {
	return &IndexData{
		SchemaVersion: SchemaVersion,
		Root:          root,
		ScannedAt:     time.Now().UnixNano(),
		Zipmods:       make(map[string]*ZipmodEntry),
		Cards:         make(map[string]*CardEntry),
		Plugins:         make(map[string]*PluginEntry),
		PluginDepMap:    make(map[string][]string),
		IsolatedZipmods: make(map[string]*ZipmodEntry),
		OriginRel:       make(map[string]string),
	}
}

// IsCompatible 判断旧索引能否复用。
//
// 条件：版本一致 + root 一致 + 三张表都非 nil。
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
	if d.Zipmods == nil || d.Cards == nil || d.Plugins == nil {
		return false
	}
	return true
}

// EnsureMaps 补齐可能为 nil 的 map，避免后续写入 panic。
func (d *IndexData) EnsureMaps() {
	if d.Zipmods == nil {
		d.Zipmods = make(map[string]*ZipmodEntry)
	}
	if d.Cards == nil {
		d.Cards = make(map[string]*CardEntry)
	}
	if d.Plugins == nil {
		d.Plugins = make(map[string]*PluginEntry)
	}
	if d.PluginDepMap == nil {
		d.PluginDepMap = make(map[string][]string)
	}
	if d.IsolatedZipmods == nil {
		d.IsolatedZipmods = make(map[string]*ZipmodEntry)
	}
	if d.OriginRel == nil {
		d.OriginRel = make(map[string]string)
	}
}
