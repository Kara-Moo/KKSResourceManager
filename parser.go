package main

// parser.go —— 资源解析层
//
// 本文件的二进制解析能力全部来自参考项目 KKCardModCheck / illusionsCard：
//
//	1. manifest.xml 解析（zipmod GUID）
//	   移植自 KKCardModCheck/util/ModXml.go 与 util/xmltools.go。
//	   包含 UTF-16 -> UTF-8 转码，以及畸形 manifest.xml 的“移除多余闭合标签”修补逻辑。
//
//	2. 卡片依赖解析（PNG 尾部数据 -> zipmod GUID）
//	   复用 github.com/GenesisAN/illusionsCard（KKCardModCheck 使用的同一个库）：
//	   util.PngBytesRead -> ic.ReadCard -> Base.Card.GetZipmodsDependencies()
//	   以及场景卡专用的 SceneExtended.itemInfo 依赖提取。
//
//	3. 兜底扫描
//	   复用 illusionsCard/Base 的 ResolveInfo msgpack 解码器，在 PNG 尾部数据中
//   直接定位 UniversalAutoResolver 写入的依赖条目，保证任何卡型都能提取到 GUID。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"

	ic "github.com/GenesisAN/illusionsCard"
	"github.com/GenesisAN/illusionsCard/Base"
	"github.com/GenesisAN/illusionsCard/KK"
	"github.com/GenesisAN/illusionsCard/util"
)

// ---------------------------------------------------------------------------
// 1. manifest.xml 解析（移植自 KKCardModCheck/util/ModXml.go）
// ---------------------------------------------------------------------------

// ModManifest 对应 zipmod 内 manifest.xml 的结构。
// 字段与参考项目 util.ModXml 保持一致，便于对照排查。
type ModManifest struct {
	GUID        string `xml:"guid"`
	Version     string `xml:"version"`
	Name        string `xml:"name"`
	Author      string `xml:"author"`
	Description string `xml:"description"`
	Website     string `xml:"website"`
	Game        string `xml:"game"`
	MD5         string `xml:"md5"`
	FileName    string `xml:"filename"`
}

// manifestDependencies 用于尽力提取 manifest.xml 中声明的 mod 间依赖。
// 非标准字段，解析失败一律忽略。
type manifestDependencies struct {
	Dependencies []struct {
		GUID string `xml:"guid"`
	} `xml:"dependencies"`
	Dependency []struct {
		GUID string `xml:"guid"`
	} `xml:"dependency"`
}

// ReadModManifest 打开 zipmod（本质是 zip），找到 manifest.xml 并解析出 GUID。
// 移植自 KKCardModCheck/util.ReadZip，并补充了“根目录内任意 xml 兜底查找”能力。
func ReadModManifest(src string) (ModManifest, error) {
	mod := ModManifest{FileName: filepath.Base(src)}

	info, err := os.Stat(src)
	if err != nil {
		return mod, fmt.Errorf("获取文件信息失败: %w", err)
	}
	if info.Size() == 0 {
		return mod, fmt.Errorf("文件大小为 0: %s", src)
	}

	zr, err := zip.OpenReader(src)
	if err != nil {
		return mod, fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer zr.Close()

	manifestFound := false
	for _, v := range zr.File {
		if v.FileInfo().Name() == "manifest.xml" {
			manifestFound = true
			if err := parseManifestEntry(v, &mod); err != nil {
				return mod, err
			}
			break
		}
	}

	if !manifestFound {
		// 兜底：部分 mod 的 manifest.xml 不叫这个名字，或放在子目录中。
		// 逐个尝试解析根目录附近的 xml，取第一个能解析出 GUID 的。
		for _, v := range zr.File {
			name := strings.ToLower(v.FileInfo().Name())
			if !strings.HasSuffix(name, ".xml") {
				continue
			}
			candidate := mod
			if err := parseManifestEntry(v, &candidate); err != nil {
				continue
			}
			if candidate.GUID != "" {
				mod = candidate
				manifestFound = true
				break
			}
		}
	}

	return mod, nil
}

// parseManifestEntry 读取 zip 中的单个条目并解析为 manifest。
func parseManifestEntry(v *zip.File, mod *ModManifest) error {
	fr, err := v.Open()
	if err != nil {
		return fmt.Errorf("打开 manifest.xml 失败: %w", err)
	}
	defer fr.Close()

	data, err := io.ReadAll(fr)
	if err != nil {
		return fmt.Errorf("读取 manifest.xml 失败: %w", err)
	}

	// 可能是 UTF-16，先转为 UTF-8
	if isLikelyUTF16(data) {
		data = utf16ToUtf8(data)
	}

	if err := xml.Unmarshal(data, mod); err != nil {
		// 解析失败，尝试“移除多余闭合标签”再解析一次
		fixed := removeUnmatchedClosingTags(string(data))
		if err2 := xml.Unmarshal([]byte(fixed), mod); err2 != nil {
			return fmt.Errorf("解析 manifest.xml 失败: %v; 修补后仍失败: %v", err, err2)
		}
	}
	return nil
}

// ReadModManifestDependencies 尽力提取 manifest.xml 中声明的 mod 间依赖 GUID。
// 用于防止隔离时移动掉被其它 mod 依赖的 zipmod。
func ReadModManifestDependencies(src string) []string {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil
	}
	defer zr.Close()

	for _, v := range zr.File {
		if strings.ToLower(v.FileInfo().Name()) != "manifest.xml" {
			continue
		}
		fr, err := v.Open()
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(fr)
		fr.Close()
		if err != nil {
			return nil
		}
		if isLikelyUTF16(data) {
			data = utf16ToUtf8(data)
		}
		var deps manifestDependencies
		if err := xml.Unmarshal(data, &deps); err != nil {
			fixed := removeUnmatchedClosingTags(string(data))
			if err2 := xml.Unmarshal([]byte(fixed), &deps); err2 != nil {
				return nil
			}
		}
		var result []string
		for _, d := range deps.Dependencies {
			if g := strings.TrimSpace(d.GUID); g != "" {
				result = append(result, g)
			}
		}
		for _, d := range deps.Dependency {
			if g := strings.TrimSpace(d.GUID); g != "" {
				result = append(result, g)
			}
		}
		return dedupeStrings(result)
	}
	return nil
}

// isLikelyUTF16 判断字节序列是否带 UTF-16 BOM。
// 移植自 KKCardModCheck/util.ModXml.go。
func isLikelyUTF16(data []byte) bool {
	return len(data) >= 2 && (data[0] == 0xFF && data[1] == 0xFE || data[0] == 0xFE && data[1] == 0xFF)
}

// utf16ToUtf8 将 UTF-16 字节转换为 UTF-8。
// 移植自 KKCardModCheck/util.ModXml.go。
func utf16ToUtf8(data []byte) []byte {
	u16 := make([]uint16, len(data)/2)
	for i := range u16 {
		u16[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
	}
	runes := utf16.Decode(u16)
	return []byte(string(runes))
}

// ---------------------------------------------------------------------------
// 2. XML 修补（移植自 KKCardModCheck/util/xmltools.go）
// ---------------------------------------------------------------------------

// reTag 匹配 4 类 XML “标签或声明”：注释、DOCTYPE、处理指令、普通标签。
var reTag = regexp.MustCompile(`(?s)<!--.*?-->|<!DOCTYPE.*?>|<\?.*?\?>|<[^>]+>`)

// removeUnmatchedClosingTags 移除 XML 中多余（无匹配开标签）的闭合标签。
// 移植自 KKCardModCheck/util/xmltools.go。
func removeUnmatchedClosingTags(xmlStr string) string {
	var stack []string
	var sb strings.Builder

	lastIndex := 0
	matches := reTag.FindAllStringIndex(xmlStr, -1)
	if matches == nil {
		return xmlStr
	}

	for _, m := range matches {
		start, end := m[0], m[1]
		sb.WriteString(xmlStr[lastIndex:start])

		tagText := xmlStr[start:end]
		if strings.HasPrefix(tagText, "<!--") ||
			strings.HasPrefix(tagText, "<!DOCTYPE") ||
			strings.HasPrefix(tagText, "<?") {
			sb.WriteString(tagText)
			lastIndex = end
			continue
		}

		content := strings.TrimSpace(tagText[1 : len(tagText)-1])
		if len(content) == 0 {
			lastIndex = end
			continue
		}

		isSelfClose := strings.HasSuffix(content, "/")
		if isSelfClose {
			content = strings.TrimSpace(content[:len(content)-1])
		}

		if content[0] == '/' {
			name := strings.TrimSpace(content[1:])
			if idx := strings.IndexAny(name, " \t\r\n"); idx != -1 {
				name = name[:idx]
			}
			if len(stack) > 0 {
				if stack[len(stack)-1] == name {
					stack = stack[:len(stack)-1]
					sb.WriteString(tagText)
				}
			}
		} else {
			name := content
			if idx := strings.IndexAny(name, " \t\r\n"); idx != -1 {
				name = name[:idx]
			}
			if !isSelfClose {
				stack = append(stack, name)
			}
			sb.WriteString(tagText)
		}
		lastIndex = end
	}

	sb.WriteString(xmlStr[lastIndex:])

	// 参考实现到此结束：它只移除多余的闭合标签。
	// 但若原文件存在“缺少闭合标签”的情况（此时栈非空），
	// 修补后的 XML 依然无法被 xml.Unmarshal 解析。
	// 此处按栈的逆序补齐剩余闭合标签，保证修补结果一定是合法 XML。
	for i := len(stack) - 1; i >= 0; i-- {
		sb.WriteString("</" + stack[i] + ">")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// 3. 卡片依赖解析
// ---------------------------------------------------------------------------

// ParseCardGUIDs 提取一张资源卡片所依赖的全部 zipmod GUID。
//
// 支持的角色卡 / 场景卡 / 服装卡 / POSE 卡 / 工作室角色卡等 PNG 资源。
// 返回值已去重并按字典序排列。
func ParseCardGUIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}
	return ParseCardGUIDsFromBytes(data, path)
}

// ParseCardGUIDsFromBytes 从已读入的 PNG 字节中提取依赖 GUID。
func ParseCardGUIDsFromBytes(data []byte, path string) ([]string, error) {
	guids := make([]string, 0, 8)

	// 3.1 走 illusionsCard 的正规解析路径
	if pgb, err := util.PngBytesRead(data); err == nil {
		pgb.FilePath = path
		if card, err := ic.ReadCard(pgb); err == nil && card != nil {
			guids = append(guids, card.GetZipmodsDependencies()...)
			// 场景卡额外携带 itemInfo 中的物品依赖
			if scene, ok := card.(*KK.KKSceneCard); ok {
				guids = append(guids, sceneItemZipmodDependencies(scene)...)
			}
		}
	}

	// 3.2 兜底：在 PNG 尾部数据中直接扫描 UniversalAutoResolver 的依赖条目
	guids = append(guids, scanResolveInfoGUIDs(data)...)

	if len(guids) == 0 {
		return []string{}, nil
	}
	return dedupeStrings(guids), nil
}

// sceneItemZipmodDependencies 提取场景卡 itemInfo 中的物品依赖。
// 移植自 KKCardModCheck/content_page/card_mod_summary.go。
func sceneItemZipmodDependencies(scene *KK.KKSceneCard) []string {
	if scene == nil || scene.SceneExtended == nil {
		return nil
	}
	plugin := scene.SceneExtended[Base.UARExtID]
	if plugin == nil {
		plugin = scene.SceneExtended[Base.UARExtIDOld]
	}
	if plugin == nil {
		return nil
	}
	data, ok := plugin.Data.(map[string]interface{})
	if !ok {
		return nil
	}
	entries, ok := data["itemInfo"].([]interface{})
	if !ok {
		return nil
	}
	dependencies := make([]string, 0, len(entries))
	for _, entry := range entries {
		encoded, ok := entry.([]byte)
		if !ok {
			continue
		}
		var resolve Base.ResolveInfo
		if rest, err := resolve.UnmarshalMsg(encoded); err != nil || len(rest) != 0 {
			continue
		}
		if guid := strings.TrimSpace(resolve.GUID); guid != "" {
			dependencies = append(dependencies, guid)
		}
	}
	return dependencies
}

// modIDMarker 是 ResolveInfo msgpack 映射中键 “ModID” 的原始字节。
var modIDMarker = []byte("ModID")

// scanResolveInfoGUIDs 在 PNG 尾部数据中定位 UniversalAutoResolver 写入的依赖条目。
//
// 实现方式：在 PNG 的 IEND 之后的追加数据里查找 “ModID” 键，
// 再用 illusionsCard/Base.ResolveInfo 的 msgpack 解码器还原出 GUID。
// 这样即便卡型不被 illusionsCard 的 readerMap 识别，也能提取到依赖。
func scanResolveInfoGUIDs(data []byte) []string {
	payload := pngPayload(data)
	if len(payload) == 0 {
		return nil
	}

	guids := make([]string, 0, 8)
	offset := 0
	for {
		rel := bytes.Index(payload[offset:], modIDMarker)
		if rel < 0 {
			break
		}
		pos := offset + rel
		offset = pos + len(modIDMarker)

		// “ModID” 之前依次可能是: 键的 fixstr 头(1字节)、map 头(1~3字节)。
		for back := 1; back <= 5; back++ {
			start := pos - back
			if start < 0 {
				continue
			}
			if !plausibleMapHeader(payload[start]) {
				continue
			}
			var info Base.ResolveInfo
			rest, err := info.UnmarshalMsg(payload[start:])
			if err != nil {
				continue
			}
			if len(rest) >= len(payload)-start {
				// 未消费任何字节，不是一个合法的映射起点
				continue
			}
			guid := strings.TrimSpace(info.GUID)
			if guid == "" {
				continue
			}
			guids = append(guids, guid)
			break
		}
	}
	return guids
}

// plausibleMapHeader 判断字节是否可能是一个 msgpack map 头。
func plausibleMapHeader(b byte) bool {
	switch {
	case b >= 0x80 && b <= 0x8f: // fixmap
		return true
	case b == 0xde || b == 0xdf: // map16 / map32
		return true
	}
	return false
}

// pngPayload 返回 PNG 的 IEND 之后追加的数据。
// 若文件不是带追加数据的 PNG，则返回空。
func pngPayload(data []byte) []byte {
	idx := bytes.Index(data, Base.PngEndChunk)
	if idx < 0 {
		return nil
	}
	end := idx + len(Base.PngEndChunk)
	if end >= len(data) {
		return nil
	}
	return data[end:]
}

// ---------------------------------------------------------------------------
// 4. 通用工具
// ---------------------------------------------------------------------------

// dedupeStrings 去重并保持稳定顺序。
func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	return result
}

// SortedGUIDs 返回排序后的 GUID 列表，用于稳定输出。
func SortedGUIDs(guids []string) []string {
	out := dedupeStrings(guids)
	sort.Strings(out)
	return out
}
