package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// StateDirName 状态目录名（位于 DELCARDS_ROOT 下）。
const StateDirName = "_state"

// IndexFileName 索引文件名。
const IndexFileName = "index.json"

// Store 负责索引文件的读写。
type Store struct {
	// dir 状态目录（DELCARDS_ROOT/_state）
	dir string
}

// NewStore 创建 Store 并确保目录存在。
func NewStore(delCardsRoot string) (*Store, error) {
	dir := filepath.Join(delCardsRoot, StateDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建状态目录失败: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir 返回状态目录路径。
func (s *Store) Dir() string { return s.dir }

// Path 返回索引文件路径。
func (s *Store) Path() string { return filepath.Join(s.dir, IndexFileName) }

// Load 加载索引。
//
// 文件不存在、损坏或版本不兼容时返回 (nil, nil) ——
// 表示「没有可复用的索引」，调用方应全量重建，而不是报错。
func (s *Store) Load(root string) (*IndexData, error) {
	data, err := os.ReadFile(s.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取索引失败: %w", err)
	}
	if len(data) == 0 {
		return nil, nil
	}

	var idx IndexData
	if err := json.Unmarshal(data, &idx); err != nil {
		// 索引损坏：丢弃并全量重建，不阻断启动
		return nil, nil
	}
	if !idx.IsCompatible(root) {
		return nil, nil
	}
	// 补齐可能为 nil 的 map，避免后续写入 panic
	if idx.Zipmods == nil {
		idx.Zipmods = make(map[string]*ZipmodEntry)
	}
	if idx.Cards == nil {
		idx.Cards = make(map[string]*CardEntry)
	}
	if idx.Plugins == nil {
		idx.Plugins = make(map[string]*PluginEntry)
	}
	if idx.PluginDepMap == nil {
		idx.PluginDepMap = make(map[string][]string)
	}
	return &idx, nil
}

// Save 原子保存索引。
//
// 流程：写 index.json.tmp → Sync → Rename 覆盖。
// 这样即使进程在写入过程中被杀，原有的 index.json 依然完整。
func (s *Store) Save(idx *IndexData) error {
	if idx == nil {
		return fmt.Errorf("索引为空，无法保存")
	}
	idx.SchemaVersion = SchemaVersion

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化索引失败: %w", err)
	}

	tmp := s.Path() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("创建临时索引文件失败: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写入索引失败: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("同步索引失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("关闭索引文件失败: %w", err)
	}

	if err := os.Rename(tmp, s.Path()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换索引文件失败: %w", err)
	}
	return nil
}
