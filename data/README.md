# 检测规则编写指南（data/）

本目录是 RemoteKnown 检测规则的**发布源**，通过 GitHub raw 提供给所有客户端拉取：

- 拉取基址（可在客户端「设置」里用配置项 `rules_update_url` 覆盖）：
  `https://raw.githubusercontent.com/samwafgo/RemoteKnown/main/data`
- 客户端「检查更新」时先读 `version.json` 比对版本号，确认要升级后再下载 `rules.json` 写入本地 SQLite 并立即生效。
- 每个版本都会保留在客户端 SQLite 中，**可随时回滚**到任意历史版本。

> 首次运行的客户端不会联网，而是把内置的默认规则（等同本目录 v1.0.0）作为种子写入 SQLite。之后才从 GitHub 拉取更新。

## 两种更新方式：自动 + 手工

| 方式 | 场景 | 操作 |
|------|------|------|
| **自动更新** | 能访问外网/内部镜像 | 设置 → 通用 → 检测规则 → 「检查更新」，从 `rules_update_url` 拉取并提示应用 |
| **手工导入** | 完全内网 / 离线 | 设置 → 通用 → 检测规则 → 「手工导入」，选择一份 `rules.json` 文件直接导入 |

手工导入用的就是与本目录**完全相同格式**的 `rules.json`（含 `version` / `minAppVersion` / `tools`）。两种方式都会经过同样的版本门槛校验，并写入 SQLite、保留可回滚。

> 手工导入要求 `version` 唯一：若导入的版本号在本机历史里已存在，会提示「版本已存在，请修改 version」。因此每次修改规则都要抬高 `version`（与自动更新一致）。

---

## 文件结构

### `version.json` — 版本元信息（先拉取它比对）

```json
{
  "version": "1.0.0",
  "minAppVersion": "1.0.0",
  "description": "初始内置检测规则",
  "updatedAt": "2026-06-16"
}
```

| 字段 | 含义 |
|------|------|
| `version` | 规则版本号（点分，如 `1.2.0`）。**每次修改 `rules.json` 必须同步抬高它**，否则客户端不会提示更新。 |
| `minAppVersion` | 此规则要求的**最低主程序（exe）版本**。客户端当前版本若低于它，会提示「请先升级主程序」并禁止应用本规则。详见下文「版本门槛」。 |
| `description` | 本次更新的简要说明，会展示给用户。 |
| `updatedAt` | 发布日期（仅展示）。 |

### `rules.json` — 规则内容

```json
{
  "version": "1.0.0",
  "minAppVersion": "1.0.0",
  "tools": [ { /* 一条规则 */ }, ... ]
}
```

`version` / `minAppVersion` 应与 `version.json` 保持一致（应用时以 `rules.json` 为准做二次校验）。`tools` 是规则数组，每个元素描述「如何判定某个远程软件正在被远程控制」。

---

## `tools[]` 字段逐项说明

| 字段 | 类型 | 含义 |
|------|------|------|
| `processName` | string | 进程名（如 `todesk.exe`，不区分大小写）。多数检测以「该进程存在」为前提。 |
| `commandLineArgs` | string[] | 命令行参数特征。**全部命中**才算被远程（如同时含 `--localPort=` 和 `--isVideoSession=true`）。 |
| `detectChildProcess` | bool | 是否检测「会话子进程」：父进程也是同名进程的派生进程（新版 ToDesk 远程会话激活时会派生一个无参数子进程）。 |
| `childProcessExcludeArgs` | string[] | 子进程检测时要排除的命令行特征，用于排除常驻的服务/主客户端进程，避免空闲误报。 |
| `windowClass` | string | 窗口类名，精确匹配。某些软件远程时会出现特定类名的窗口。 |
| `windowTitle` | string | 窗口标题，**包含**匹配（如「聊天」）。 |
| `tcpConnThreshold` | int | TCP 连接数阈值，进程连接数 **≥** 此值即判定被远程（0 表示不检测）。 |
| `useEstablishedOnly` | bool | TCP 检测时是否只统计 `ESTABLISHED` 状态的连接。 |
| `udpConnThreshold` | int | UDP 连接数阈值，进程连接数 **>** 此值即判定被远程（0 表示不检测）。 |
| `toolName` | string | 工具显示名称（如 `ToDesk`、`向日葵`），会展示在状态与历史里。 |

### 检测优先级

对每个工具，按以下顺序判定，命中即停止（与守护进程 `DetectRemoteTools` 逻辑一致）：

```
命令行参数 → 会话子进程 → 窗口类名 → 窗口标题 → TCP连接数 → UDP连接数 → 进程存在
```

> 「进程存在」是兜底：仅当一个工具**只**填了 `processName`、其余检测项都为空时，进程一出现就判定被远程。

---

## 新增一个工具的步骤

1. 在 `tools` 数组里追加一条规则，挑选最可靠的检测手段（优先命令行参数）。
2. 抬高 `version.json` 和 `rules.json` 的 `version`（如 `1.0.0` → `1.0.1`）。
3. 若这条规则用到了**旧客户端不支持的新指标字段**（见下「版本门槛」），把 `minAppVersion` 设为支持该指标的主程序版本。
4. 提交并推送到 `main` 分支，客户端即可「检查更新」拉到。

完整示例：

```json
{
  "version": "1.0.1",
  "minAppVersion": "1.0.0",
  "tools": [
    {
      "processName": "todesk.exe",
      "commandLineArgs": ["--localPort=", "--isVideoSession=true"],
      "detectChildProcess": true,
      "childProcessExcludeArgs": ["--runservice", "--localPort", "--isVideoSession", "--hide"],
      "toolName": "ToDesk"
    },
    {
      "processName": "新软件.exe",
      "windowTitle": "远程协助中",
      "toolName": "某新远程工具"
    }
  ]
}
```

---

## 版本门槛（minAppVersion）

规则用 JSON，旧客户端遇到**未知字段会直接忽略**——这意味着如果你用了一个新增的检测指标，旧客户端不会报错，但会**静默漏检**。为避免这种「看起来更新成功、实际检测失效」的情况：

- 规则文件声明 `minAppVersion`；
- 客户端「检查更新」时若**自身版本 < minAppVersion**，会提示「请先升级主程序到 vX，再升级规则」，并禁止直接应用；
- 只有把主程序升级到位后，才允许应用这条规则。

**因此新增指标的正确流程是：先发布支持新指标的主程序版本 → 再发布把 `minAppVersion` 抬到该版本的规则。**

---

## 未来可扩展指标（roadmap，暂未实现）

下列指标是规划方向，**当前版本尚未实现**，列出以便提前规划规则结构。落地任意一个时，都要遵循上面的「先发新 exe、再抬 minAppVersion」流程：

| 计划字段 | 设想含义 |
|----------|----------|
| `serviceName` | 按 Windows 服务名检测 |
| `parentProcessName` | 父进程名匹配 |
| `filePathContains` / `fileVersion` | 按可执行文件路径片段 / 文件版本检测 |
| `cmdlineRegex` | 命令行正则匹配（比 `commandLineArgs` 更灵活） |
| `windowCountThreshold` | 窗口数量阈值 |
| `remoteIP` | 已建立连接的远端 IP 特征 |
