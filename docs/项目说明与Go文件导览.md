# KKS 资源管理器 · 项目说明与 Go 文件导览

> 版本：**v2.4**（2026-11-14）
> 仓库：<https://github.com/Kara-Moo/KKSResourceManager>
> 可执行文件：`dist/KKSResourceManager.exe`

---

## 1. 项目简介

**KKS 资源管理器（KKS Resource Manager）** 是一个面向 **Koikatsu Sunshine（KKS / KK 系）** 玩家的小型 Windows 桌面工具，用来管理游戏目录里散落的角色卡、场景卡、POSE、服装等资源文件。

它解决的核心问题：

1. **资源太乱**：卡片散落在 `UserData/chara/female` 等目录，想临时“收起来”又不想真删；
2. **怕删坏**：直接删文件不可逆，且容易破坏目录结构；
3. **看得见才敢动**：需要缩略图、文件夹统计、右侧预览来判断哪些资源该隔离。

因此工具提供：

- **安全加载隔离模式**：隔离 = 把资源移动到 `DELCARDS/DEL-<类型>/<原相对路径>`；启用 = 移回原位。游戏目录里不会真正删除资源。
- **自由修改模式**：像资源管理器一样复制 / 剪切 / 粘贴 / 新建 / 重命名 / 删除（删除进回收站）。
- **回收站**：删除只移动到 `DELCARDS/DELED`，可还原；彻底删除需要二次确认并输入名称。
- **隐藏的文件夹**：隐藏空文件夹但保留原始目录树，可一键恢复。
- **缩略图与右侧面板**：后端解码 PNG/JPG、缩放后以 data URI 返回，前端渲染九宫格 / 大图 / 信息栏。
- **事务与撤销**：所有写操作走事务 journal，失败可回滚，已提交操作可撤销。

### v2.4 的关键变化

| 变化 | 说明 |
|---|---|
| **彻底移除 zipmod 管理** | 删除 zipmod / 插件 / GUID 依赖图相关能力；直接粘贴 `.zipmod` 会被拒绝；历史遗留 zipmod 回收站条目只展示、不可操作 |
| **索引 schema v1 → v2** | 索引只保存卡片指纹（size + mtime），扫描只 `stat`、不解析 PNG / GUID；冷启动大幅提速 |
| **文件夹迁移引擎重写** | 先规划、再动盘；文件夹内所有普通文件都迁移；空文件夹明确报错；失败回滚连目录一起回收；迁移后清理源侧空壳 |

### 技术栈

| 项目 | 内容 |
|---|---|
| 语言 | Go 1.27.4 |
| 桌面框架 | Wails v2.17.1（Go ↔ 前端 JS 双向调用） |
| 前端 | 纯静态 `frontend/index.html`，内联 HTML/CSS/JS，**无前端构建步骤** |
| 渲染 | Windows WebView2（随 Edge 提供） |
| 平台 | Windows amd64 |
| 主要依赖 | Wails v2、`golang.org/x/image`、`golang.org/x/sys` |

---

## 2. 核心概念与磁盘约定

### 2.1 两个根目录

```
<KKS_ROOT>/                        例如 E:\KKV114
├── UserData/
│   ├── chara/female                女性角色卡
│   ├── chara/male                  男性角色卡
│   ├── Studio/scene                场景卡
│   ├── Studio/pose                 POSE 动作
│   └── coordinate/{female,male}    女性 / 男性服装
└── ...

<DELCARDS_ROOT>/                   固定为 KKS_ROOT 的**同级目录**，例如 E:\DELCARDS
├── DEL-female_card/                隔离区（保留原相对路径）
├── DEL-male_card/
├── DEL-scene/
├── DEL-pose/
├── DEL-coord_f/
├── DEL-coord_m/
├── DELED/                          回收站（删除的资源）
├── DEL-invisible/                  隐藏文件夹状态（KKSinvisible.json 等）
├── _state/                         索引、缩略图缓存
│   ├── index.json
│   └── thumb/
└── _journal/                       事务日志（崩溃恢复）
```

### 2.2 六种受管资源

| Mode | 中文名 | 源目录（相对 KKS_ROOT） | 隔离目录 | 扩展名 |
|---|---|---|---|---|
| `female_card` | 角色卡（女） | `UserData/chara/female` | `DEL-female_card` | `.png` |
| `male_card` | 角色卡（男） | `UserData/chara/male` | `DEL-male_card` | `.png` |
| `scene` | 场景卡 | `UserData/Studio/scene` | `DEL-scene` | `.png` |
| `pose` | POSE 动作 | `UserData/Studio/pose` | `DEL-pose` | `.png` |
| `coord_f` | 女性服装 | `UserData/coordinate/female` | `DEL-coord_f` | `.png` |
| `coord_m` | 男性服装 | `UserData/coordinate/male` | `DEL-coord_m` | `.png` |

> 实际文件识别支持 `.png` / `.jpg` / `.jpeg`（见 `internal/core.IsCardFile`）。
> 「工作室角色」`UserData/Studio/character` 不参与扫描，也不创建隔离目录。

### 2.3 两种工作模式

- **安全加载隔离模式**：隔离 / 启用；不提供新建、重命名、删除入口。
- **自由修改模式**：复制 / 剪切 / 粘贴、新建文件夹、重命名、删除进回收站。

### 2.4 安全红线

1. **只动用户勾选的内容**，绝不一键清理未选中的文件。
2. **隔离保留原相对目录结构**（`DEL-<kind>/<相对路径>`）。
3. **文件夹迁移 = 整棵子树**：所有普通文件都迁，不只卡片。
4. **空文件夹隔离必须报错**：不生成空目录结构。
5. **失败可回滚**：文件移回、已建目录回收（事务 journal）。
6. **删除只进回收站**：`DELED`，唯一例外是回收站“彻底删除”（二次确认 + 输入名称）。
7. **状态绝不写进 KKS_ROOT**：索引 / 事务 / 缩略图全部在 `DELCARDS/_state`、`_journal`。
8. **不再对 zipmod 执行文件操作**：直接粘贴 `.zipmod` 拒绝；历史 `DEL-zipmod` 只跳过、不改动。

---

## 3. 代码结构总览

```
KKSResourceManager/
├── main.go                    Wails 程序入口
├── app.go                     Wails 绑定层：扫描编排、树数据、事件广播
├── manager.go                 核心引擎：资源类型、扫描、隔离 / 恢复、目录管理
├── transfer.go                文件夹迁移引擎（规划 → 执行 → 回滚）
├── clipboard.go               自由模式复制 / 剪切 / 粘贴
├── crudops.go                 删除 / 回收站 / 新建 / 重命名接入层
├── panel.go                   右侧面板组装
├── panel_support.go           面板辅助方法（封面、代数、缓存）
├── folderstats.go             文件夹统计的一次性聚合（避免 O(n²)）
├── preview.go                 缩略图解码 / 缩放 / data URI
├── thumbs.go                  缩略图队列初始化与生成器
├── invisible.go               空文件夹隐藏与路径重建
├── logring.go                 后端日志环形缓冲
├── txwrap.go                  Manager ↔ 事务引擎的包装、历史 / 撤销
├── diskfree_windows.go         Windows 磁盘剩余空间检查
├── diskfree_other.go          非 Windows 平台的空实现
│
├── internal/
│   ├── core/                  排序、目录快照、封面缓存、扩展名判定
│   ├── fsx/                   文件名校验与路径安全
│   ├── index/                 索引持久化与增量扫描（schema v2）
│   ├── ops/                   回收站与自由模式 CRUD 原语
│   ├── preview/               缩略图两级缓存 + 有界并发队列
│   └── tx/                    事务引擎、journal、历史与撤销
│
├── frontend/
│   ├── index.html             唯一前端文件（内联 JS/CSS）
│   └── wailsjs/               Wails 自动生成的绑定桩
├── tests/frontend.test.mjs    Playwright + 本机 Edge 的前端端到端测试
├── tools/
│   ├── check-undefined.mjs    静态检查前端“调用了但未定义”的函数
│   └── publish-github.py      通过 GitHub Git Data API 推送的备用脚本
├── docs/                      规格书、移交说明书、本文件
├── dist/KKSResourceManager.exe 最新可执行成品
└── reference/                 参考实现（不参与编译）
```

---

## 4. 根包 Go 文件详解

> 根包 `package main` 同时包含 Wails 应用与核心引擎；`internal/*` 是可独立测试的基础设施。

### `main.go` —— 程序入口

- 初始化 `App`，调用 `wails.Run`；
- 通过 `//go:embed all:frontend` 把整个前端目录打进可执行文件；
- 窗口标题「KKS 资源管理器」，默认 1440×900；
- 把 `App` 绑定给前端（`window.go.main.App.*`）。

### `app.go` —— Wails 绑定层

- 定义 `App`，持有 `Manager`、扫描状态、日志环形缓冲；
- 对外方法统一返回 JSON 字符串，前端直接 `JSON.parse`；
- 负责扫描编排：`SelectKKSRoot` / `Rescan` / `startScan`，通过事件广播进度：
  - `scan:progress`、`scan:done`、`scan:complete`、`panel:invalidate`、`log` 等；
- 构建左侧 / 右侧树数据（`TreeNode`）、回收站、隐藏文件夹、历史记录等绑定；
- 把后端日志写入 `logRing` 并推给前端；
- 处理「在资源管理器中打开」等系统调用。

### `manager.go` —— 核心引擎

- 定义 `ResourceKind` / `ResourceKinds`、`Manager` 结构；
- `NewManager`：校验 KKS 根目录、创建 `DELCARDS` 与 6 个标准隔离目录；
- `ScanAll`：加载旧索引 → 单阶段扫描 → 原子保存 → 写入内存；
- `Isolate` / `Restore`：**先规划再动盘**，文件夹整树迁移，失败整体回滚；
- `classifyResource` / `classifyDelResource`：判断文件属于哪种资源及相对路径；
- `cleanEmptyDirs`：清理隔离区空目录（保护 6 个标准目录与 `DEL-zipmod`）；
- `ValidateRoot`：检查必需目录（`coordinate/male` 属可选）；
- 负责缩略图队列、快照缓存、封面缓存等运行时组件初始化。

### `transfer.go` —— 文件夹迁移引擎（v2.4 新增）

- `planFolderTransfer`：**纯只读规划**。`WalkDir` 整棵子树，不过滤扩展名；
  - 每个目录（含空目录）生成 `OpMkdir`；
  - 每个普通文件生成 `OpMove`，记录 size + mtime 供执行前复核；
  - 符号链接记入 `Skipped`；
  - `Files == 0` → 返回 `errEmptyFolder`，**不生成任何 Step**；
  - 源侧每个目录生成 `OpRmdir`（子先父后，边界目录不删）。
- `planFileTransfer`：单个文件的迁移计划，并补齐目标侧缺失父目录。
- `execTransferPlan`：按计划执行（建目录 → 移动 → 清理空目录），
  任一步失败调用 `tx.Rollback()`，文件移回、已建目录回收；
- `dedupeSelectionPaths`：父子同时选中时只保留祖先，避免同一文件规划两次；
- `ensureDirPath`：目标目录若曾被“隐藏”删除，按隐藏索引重建。

### `clipboard.go` —— 复制 / 剪切 / 粘贴

- 剪贴板状态：操作类型、路径、来源侧（KKS / DEL）、资源类型；
- `Paste`：预检目标同名冲突，任一冲突则整批取消；
- 剪切 = 移动，复制 = 递归复制整棵目录树；
- **硬规则**：遇到 `.zipmod` 直接拒绝，不移动、不复制。

### `crudops.go` —— 删除与回收站接入层

- 把 `internal/ops` 的 CRUD 能力挂到 `Manager`，并提供 Wails 绑定；
- `PlanDelete` / `CommitDelete`：删除 = 移动到 `DELED/<种类>/<原相对路径>`；
- `CmdTrashRestore` / `CmdTrashPurge`：回收站还原 / 彻底删除；
- `filterHistoricalZipmodTrash`：历史遗留 zipmod 条目只展示、禁止还原 / 彻底删除；
- 删除操作走事务，可撤销。

### `panel.go` —— 右侧面板

- 一次请求返回整个面板（前端不允许发 N 个请求拼面板）；
- 根据焦点是文件夹 / 卡片 / 空目录，组装不同 `Section`：
  - 文件夹：子文件夹九宫格 + 卡片九宫格 + 统计；
  - 卡片：单张大图 + 信息栏（大小等）；
  - 空目录：空态文案；
- 缩略图只查缓存，未命中返回 `pending`，由前端后续补图；
- 所有数据来自内存索引或带缓存的目录列举，**不做 PNG 解码**。

### `panel_support.go` —— 面板辅助

- 面板代数 `panelGenSeq`：前端据此丢弃过期响应；
- `pickCover`：目录封面卡选择（一级优先、深度 3、带缓存，不全树遍历）；
- 其他面板需要的 Manager 辅助方法，避免 `panel.go` 过长。

### `folderstats.go` —— 文件夹统计聚合

- 一次后序遍历得到每个目录的：直接子文件夹数、直接文件数、递归文件数、递归大小；
- 替代旧实现「每个文件夹递归统计」的 O(n²) IO；
- 结果同时供左右列表、文件夹预览使用。

### `preview.go` —— 缩略图解码

- WebView 不允许 `file://` 直接加载本地绝对路径，因此后端读图 → 缩放 → JPEG → base64 data URI；
- 需缩放后再编码：KKS 卡片可能超过 40MB，直接编码会拖垮 WebView；
- 支持 PNG / JPEG / GIF 解码；
- 限制像素数，防止恶意 / 异常大图导致内存爆炸；
- 提供卡片预览与文件夹预览两种入口。

### `thumbs.go` —— 缩略图队列

- 启动两级缓存 + 有界并发（≤3）的缩略图生成队列；
- UI 请求路径只查缓存，**绝不同步解码**；
- 冷启动不预生成全部缩略图，只按需生成当前聚焦的几张；
- `StartThumbQueue` 幂等：`startup` 与 `SelectKKSRoot` 都可调用。

### `invisible.go` —— 隐藏文件夹

- 隐藏的是**目录树**，不是单个文件；
- 状态文件位于 `DELCARDS/DEL-invisible/`：
  - `KKSinvisible.json` / `DELinvisible.json`：被隐藏的文件夹（相对各自根）；
  - `OriginKKS.json` / `OriginDEL.json`：原始目录树；
- 隐藏前校验递归文件数为 0（非空文件夹拒绝隐藏）；
- `EnsurePathForOperation`：隔离 / 恢复前若目标父目录曾被隐藏删除，按索引重建；
- 恢复时按记录的目录树还原整棵子树。

### `logring.go` —— 日志环形缓冲

- 容量 5000 条的环形日志；
- 每条日志有全局递增 `seq`，前端按 `seq` 拉增量；
- 刷新 / 重开窗口不丢日志；
- 级别：`info / success / warn / error`；作用域：`scan / isolate / paste / index / ui` 等。

### `txwrap.go` —— 事务包装

- `beginTx` / `commitTx` / `rollbackTx`：Manager 与 `internal/tx` 的桥接；
- `txMove` / `txCopy`：写操作走事务（无事务时退化为直接操作）；
- `GetHistory` / `CmdUndo`：读取操作历史、撤销已提交事务；
- `RecoverPendingTx`：启动时自动回滚未完成事务（崩溃恢复）。

### `diskfree_windows.go` / `diskfree_other.go`

- Windows 下通过 `golang.org/x/sys/windows` 查询目标磁盘剩余空间；
- 跨盘复制前检查空间，避免复制到一半失败留下半成品；
- 非 Windows 平台提供空实现，保证可编译。

---

## 5. internal 包详解

### `internal/core` —— 基础设施

| 内容 | 说明 |
|---|---|
| `NaturalLess` / `SortNatural` | 自然序排序（`1, 2, 10` 而不是 `1, 10, 2`） |
| `SnapshotCache` | 目录快照 LRU（默认 128），单次面板读取 ≤2 次 `ReadDir` |
| `CoverCache` | 封面卡缓存 LRU（默认 512） |
| `PickCover` | 选目录封面卡：一级优先 → 深度 3 / 最多 64 目录 BFS |
| `IsCardFile` | 统一判定 `.png` / `.jpg` / `.jpeg` |
| `ParseSize` | 字节数格式化为人类可读大小 |

### `internal/fsx` —— 文件系统安全

- `ValidateName`：文件名合法性校验（非法字符、保留名、扩展名变化等）；
- `UniqueFolderName`：新建文件夹时生成不重名名称；
- `PathWithin`：判断路径是否位于根目录内，防止路径穿越；
- 前后端各校验一次：前端即时反馈，后端最终把关。

### `internal/index` —— 索引引擎（schema v2）

| 文件 | 职责 |
|---|---|
| `types.go` | `IndexData` / `CardEntry` / `Fingerprint`；`SchemaVersion = 2`；紧凑持久化编码（`fp:"size:mtime"` + `k/r` 字段，兼容旧 v2 对象形式） |
| `store.go` | 索引读写：`DELCARDS/_state/index.json`；Load 对 v1 返回 `Upgraded` 且不删旧文件；Save 用 `.tmp → Sync → Rename` 原子覆盖 |
| `scan.go` | 单阶段扫描：只 `stat` 6 个资源目录下的卡片文件，按 size + mtime 指纹 diff，复用 / 新增 / 删除；**不解析 PNG、不读 DLL、不解析 zipmod** |

> 索引只保存正向数据（卡片指纹 + 类型 + 相对路径），不再有 zipmod / plugin / 依赖图。

### `internal/ops` —— 回收站与 CRUD

| 文件 | 职责 |
|---|---|
| `trash.go` | `TrashStore`：`DELED` 元数据、`UniqueTrashPath`、大小统计；记录 `OriginalAbs` 以便还原 |
| `crud.go` | 新建文件夹、重命名、`PlanDelete` / `CommitDelete`、还原、彻底删除；所有写操作走 `tx.Tx` |

### `internal/preview` —— 缩略图缓存与队列

- 两级缓存：内存 + 磁盘（`DELCARDS/_state/thumb`）；
- 有界并发 worker（≤3），支持 `SizeGrid`（九宫格）与 `SizeLarge`（大图）两档；
- 队列按 `thumbKey` 去重，避免同一文件重复生成；
- 未命中返回 `pending`，生成完成后通过批量事件通知前端补图。

### `internal/tx` —— 事务引擎

| 文件 | 职责 |
|---|---|
| `tx.go` | `Begin` / `Move` / `Copy` / `Mkdir` / `RmdirIfEmpty` / `Rename` / `Commit` / `Rollback`；每步先写 intent 到 journal，再执行；步数上限 500 |
| `history.go` | 操作历史（最近 200 条）、撤销（逐步骤指纹校验）、已撤销状态标记 |

- Journal 目录：`DELCARDS/_journal/<txId>/journal.json`；
- 崩溃启动时 `RecoverPending` 自动逆序回滚；
- 回滚支持：文件移回、副本删除、空目录删除 / 重建、回收站还原。

---

## 6. 测试文件导览

### 根包测试

| 文件 | 覆盖内容 |
|---|---|
| `manager_test.go` | Manager 初始化、分类、隔离 / 恢复基本行为、同名冲突、移动原语 |
| `transfer_test.go` | **文件夹迁移引擎 T1–T9**：整树迁移、空子目录、混合内容、冲突、回滚、重复隔离、多选、父子重叠、空文件夹、撤销 |
| `feature_test.go` | 文件夹整体隔离 / 恢复、隐藏空文件夹、路径重建、复制 / 剪切 / 粘贴 |
| `txwrap_test.go` | 事务历史、撤销、失败回滚、崩溃恢复 |
| `e2e_test.go` | 扫描 → 索引 → 资源计数链路 |
| `concurrency_test.go` | 后台扫描与前端读取的并发安全（配合 `-race`） |
| `perf_test.go` | 构建树耗时、文件夹统计聚合正确性 |
| `panel_test.go` | 面板九宫格 / 卡片 / 空目录 / 自然序 / 封面 / 信息栏 |
| `preview_test.go` | 预览缩放、非法路径、非图片、根目录校验 |
| `thumbs_test.go` | 缩略图队列、去重、缓存命中 |
| `cardimage_test.go` | 大图 / 透明图 / 尾部附加数据的预览 |
| `cardjpg_test.go` | JPG 扩展名识别、JPG 预览、资源管理器路径解析 |
| `derive_test.go` | 隐藏路径归一化、绝对 / 相对路径匹配 |
| `realworld_test.go` | 真实 KKS 目录验证（需 `KKS_REAL_ROOT`） |
| `realthumb_test.go` | 真实卡片缩略图抽样（需 `KKS_REAL_ROOT`） |

### internal 包测试

- `internal/core/core_test.go`：自然序、快照缓存、封面选择；
- `internal/fsx/name_test.go`：文件名校验；
- `internal/index/index_test.go`：v1→v2 迁移、指纹复用、删除检测、紧凑编码、旧 v2 兼容；
- `internal/ops/ops_test.go`：回收站计划 / 提交 / 还原 / 彻底删除；
- `internal/preview/preview_test.go`：内存 / 磁盘缓存、队列去重；
- `internal/tx/tx_test.go`：移动 / 复制 / 目录 / 重命名的提交与回滚、journal 恢复、历史撤销。

### 前端端到端

`tests/frontend.test.mjs` 使用 **Playwright + 本机 Edge** 加载 `frontend/index.html`，
注入模拟的 Wails 后端，覆盖：

- 单击 / 双击 / 右键菜单 / 全选；
- 自由模式按钮可见性与回收站；
- 面板九宫格、缩略图补图；
- 隐藏文件夹、JPG 识别；
- 右键隔离 / 启用、多选文件夹只传顶层路径；
- 历史窗口与撤销。

运行：

```bash
node tests/frontend.test.mjs
```

---

## 7. 前端与工具链

### `frontend/index.html`

- 唯一前端文件，内联 CSS 与 JS，无框架、无构建；
- 通过 `window.go.main.App.<方法>` 调后端；
- 订阅后端事件：`scan:progress`、`scan:done`、`scan:complete`、`panel:invalidate`、`log`、`thumb:ready`；
- 后端绑定桩由 `wails build` 生成到 `frontend/wailsjs/`。

### `tools/check-undefined.mjs`

- 静态扫描内联 JS 中「调用了但从未定义」的函数名；
- 这类错误会被 async 函数吞掉，表现为“点了没反应”；
- 改前端后必跑：

```bash
node tools/check-undefined.mjs
```

### `tools/publish-github.py`

- 通过 GitHub **Git Data API** 提交，适用于本机 git 协议被代理阻断的场景；
- 需要环境变量 `GITHUB_TOKEN`；
- 正常网络环境下直接用 `git push` 即可。

### `docs/`

- `v2.4-移除zipmod与迁移引擎重写提示词.md`：v2.4 施工规格；
- `v2.4-工作移交说明书.md`：交接说明、进度、验收清单；
- 本文件：项目说明与 Go 文件导览。

---

## 8. 构建、运行与测试

```bash
# 编译（产物：build/bin/KKSResourceManager.exe）
wails build

# 复制交付成品
cp build/bin/KKSResourceManager.exe dist/KKSResourceManager.exe

# Go 全量测试
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1

# 前端静态检查
node tools/check-undefined.mjs

# 前端端到端（需要本机 Edge + Playwright）
node tests/frontend.test.mjs

# 真实目录测试（可选）
KKS_REAL_ROOT="E:\KKV114" go test -run TestReal -v
```

---

## 9. v2.4 验证数据（本机 E:\KKV114，12973 张卡）

| 指标 | 实测 |
|---|---|
| 冷启动（无索引全量重建） | **67 ms** |
| 冷启动（v1→v2 迁移场景） | **231 ms** |
| 热启动（全部指纹复用、解析 0） | **55 ms** |
| `index.json` 体积 | **1,963,771 B ≈ 1.87 MiB** |
| 文件夹规划（`抽卡游戏`：74 文件 / 33 目录 / 801 MB） | **2 ms**，规划零副作用 |
| Go 测试 | 全包通过；`-race` 无数据竞争 |
| 前端端到端 | **100 通过 / 0 失败** |

---

## 10. 许可证

MIT License，见仓库根目录 [`LICENSE`](../LICENSE)。
