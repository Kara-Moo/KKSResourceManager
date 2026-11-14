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

// LoadResult 是 Load 的结果。
type LoadResult struct {
	// Index 可复用的索引；nil 表示需要全量重建。
	Index *IndexData
	// OldVersion 读到的旧索引版本号（0 表示文件不存在/损坏/无版本信息）。
	OldVersion int
	// Upgraded true 表示读到的是旧版本（v1），已作废、需全量重建。
	// 旧文件不会被删除，只会被后续 Save 覆盖。
	Upgraded bool
}

// Load 加载索引。
//
//   - 文件不存在、损坏 → LoadResult{}，调用方全量重建；
//   - schemaVersion 不匹配（如 v1）→ Upgraded=true、Index=nil；
//   - 版本与 root 兼容 → Index 可用。
func (s *Store) Load(root string) (LoadResult, error) {
	data, err := os.ReadFile(s.Path())
	if err != nil {
		if os.IsNotExist(err) {
			return LoadResult{}, nil
		}
		return LoadResult{}, fmt.Errorf("读取索引失败: %w", err)
	}
	if len(data) == 0 {
		return LoadResult{}, nil
	}

	var idx IndexData
	if err := json.Unmarshal(data, &idx); err != nil {
		// 索引损坏：丢弃并全量重建，不阻断启动
		return LoadResult{}, nil
	}
	if idx.SchemaVersion != SchemaVersion {
		// 旧版本（v1）：丢弃内存数据，按全新索引重建。
		// **旧文件保留在磁盘上**，由后续 Save 原子覆盖。
		return LoadResult{OldVersion: idx.SchemaVersion, Upgraded: true}, nil
	}
	if !idx.IsCompatible(root) {
		return LoadResult{}, nil
	}
	idx.EnsureMaps()
	return LoadResult{Index: &idx, OldVersion: idx.SchemaVersion}, nil
}

// Save 原子保存索引。
//
// 流程：写 index.json.tmp → Sync → Rename 覆盖。
// 这样即使进程在写入过程中被杀，原有的 index.json 依然完整。
//
// 使用紧凑 JSON（非 Indent）：16150 张卡时体积可压到 ≤2MB。
func (s *Store) Save(idx *IndexData) error {
	if idx == nil {
		return fmt.Errorf("索引为空，无法保存")
	}
	idx.SchemaVersion = SchemaVersion

	data, err := json.Marshal(idx)
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
