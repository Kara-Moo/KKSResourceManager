// Package ops 实现自由模式的文件操作：新建、重命名、删除（进回收站）、还原、彻底删除。
//
// 重构规格 §6.4 的红线 N4：
//
//	删除只进 DELED，永不 os.RemoveAll 资源。
//	唯一例外：回收站「彻底删除」（二次确认 + 输入名称）。
//
// 目录约定：
//
//	DELCARDS/DELED/<spec>/<原相对路径>   保留结构
//	DELCARDS/DELED/_trash.json            回收站元数据
package ops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DeletedDirName 回收站目录名（位于 DELCARDS_ROOT 下）。
const DeletedDirName = "DELED"

// TrashMetaFile 回收站元数据文件名。
const TrashMetaFile = "_trash.json"

// TrashMeta 一条回收站记录。
//
// 必须记录 OriginalAbs：zipmod 在 mods 下可能是嵌套路径
// （mods/sub/a.zipmod），删进 DELED/zipmod/a.zipmod 后就丢失了原路径，无法还原。
type TrashMeta struct {
	OriginalAbs string `json:"originalAbs"`
	Spec        string `json:"spec"`
	TxID        string `json:"txId"`
	DeletedAt   int64  `json:"deletedAt"`
	Size        int64  `json:"size"`
	IsDir       bool   `json:"isDir"`
}

// TrashStore 回收站元数据存储。
type TrashStore struct {
	// dir DELED 目录
	dir string
}

// NewTrashStore 创建回收站存储并确保目录存在。
func NewTrashStore(delCardsRoot string) (*TrashStore, error) {
	dir := filepath.Join(delCardsRoot, DeletedDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建回收站目录失败: %w", err)
	}
	return &TrashStore{dir: dir}, nil
}

// Dir 返回 DELED 目录。
func (s *TrashStore) Dir() string { return s.dir }

// MetaPath 返回元数据文件路径。
func (s *TrashStore) MetaPath() string { return filepath.Join(s.dir, TrashMetaFile) }

// Load 读取全部回收站记录。
func (s *TrashStore) Load() (map[string]TrashMeta, error) {
	data, err := os.ReadFile(s.MetaPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]TrashMeta{}, nil
		}
		return nil, fmt.Errorf("读取回收站元数据失败: %w", err)
	}
	if len(data) == 0 {
		return map[string]TrashMeta{}, nil
	}
	m := make(map[string]TrashMeta)
	if err := json.Unmarshal(data, &m); err != nil {
		// 损坏则丢弃，不阻断启动
		return map[string]TrashMeta{}, nil
	}
	return m, nil
}

// Save 保存回收站记录（原子写）。
func (s *TrashStore) Save(m map[string]TrashMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化回收站元数据失败: %w", err)
	}
	tmp := s.MetaPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.MetaPath())
}

// Put 记录一条回收站条目并保存。
func (s *TrashStore) Put(trashRel string, meta TrashMeta) error {
	m, err := s.Load()
	if err != nil {
		return err
	}
	m[trashRel] = meta
	return s.Save(m)
}

// Remove 移除一条回收站记录并保存。
func (s *TrashStore) Remove(trashRel string) error {
	m, err := s.Load()
	if err != nil {
		return err
	}
	delete(m, trashRel)
	return s.Save(m)
}

// List 返回排序后的回收站条目。
func (s *TrashStore) List() ([]TrashEntry, error) {
	m, err := s.Load()
	if err != nil {
		return nil, err
	}
	result := make([]TrashEntry, 0, len(m))
	for rel, meta := range m {
		result = append(result, TrashEntry{
			Rel:         rel,
			Abs:         filepath.Join(s.dir, filepath.FromSlash(rel)),
			OriginalAbs: meta.OriginalAbs,
			Spec:        meta.Spec,
			DeletedAt:   meta.DeletedAt,
			Size:        meta.Size,
			IsDir:       meta.IsDir,
		})
	}
	// 新的在前
	sort.Slice(result, func(i, j int) bool { return result[i].DeletedAt > result[j].DeletedAt })
	return result, nil
}

// TrashEntry 回收站列表项。
type TrashEntry struct {
	// Rel 相对 DELED 的路径（POSIX）
	Rel string `json:"rel"`
	// Abs DELED 下的绝对路径
	Abs string `json:"abs"`
	// OriginalAbs 删除前的绝对路径
	OriginalAbs string `json:"originalAbs"`
	// Spec 种类（female_card / zipmod / ...）
	Spec string `json:"spec"`
	// DeletedAt 删除时间
	DeletedAt int64 `json:"deletedAt"`
	// Size 大小
	Size int64 `json:"size"`
	// IsDir 是否为目录
	IsDir bool `json:"isDir"`
	// Name 显示名
	Name string `json:"name"`
}

// SpecDir 返回某种类在 DELED 下的目录。
func (s *TrashStore) SpecDir(spec string) string {
	return filepath.Join(s.dir, spec)
}

// UniqueTrashPath 为回收站内的一个条目生成不冲突的路径。
//
// zipmod 是扁平结构，重名加 __2。
func (s *TrashStore) UniqueTrashPath(spec, name string) string {
	base := filepath.Join(s.SpecDir(spec), name)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i <= 99; i++ {
		candidate := filepath.Join(s.SpecDir(spec), fmt.Sprintf("%s__%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return filepath.Join(s.SpecDir(spec), fmt.Sprintf("%s__%d%s", stem, int(time.Now().UnixNano()%100000), ext))
}

// DirSize 统计目录大小（递归）。
func DirSize(path string) int64 {
	var size int64
	_ = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		size += info.Size()
		return nil
	})
	return size
}
