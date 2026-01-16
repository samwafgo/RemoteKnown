<div align="center">
  <img src="web/assets/logo.png" width="120" alt="RemoteKnown Logo">
  <h1>RemoteKnown (远程知道了)</h1>
  <p>
    <b>本地终端远程行为感知与审计系统</b>
  </p>
  <p>
    让用户"清楚知道自己是否、何时、正在被远程控制"，保护隐私安全。
  </p>

  <p>
    <a href="README.md">简体中文</a> | <a href="README_en.md">English</a>
  </p>

  <p>
    <a href="https://github.com/samwafgo/RemoteKnown/blob/main/LICENSE">
      <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License">
    </a>
    <img src="https://img.shields.io/badge/platform-Windows-0078D6.svg" alt="Platform">
    <img src="https://img.shields.io/badge/Go-1.21+-00ADD8.svg" alt="Go">
    <img src="https://img.shields.io/badge/Electron-28+-47848F.svg" alt="Electron">
  </p>
</div>

---

## 📖 项目简介

**远程知道了** 是一款能够实时监测本地系统的远程控制状态，识别多种主流远程工具（如 ToDesk, 向日葵, 网易UU远程，AskLink远程，远程看看等），并提供桌面通知、飞书/钉钉告警以及详细的会话审计记录。

## 📸 界面预览

### 主界面状态
| 安全状态 | 正在被远程 |
| :---: | :---: |
| <img src="docs/images/RemoteKnown主界面.png" alt="安全状态" width="400"> | <img src="docs/images/RemoteKnown正在被远程的主界面.png" alt="被远程状态" width="400"> |

### 桌面通知
| 远程开始告警 | 远程结束通知 |
| :---: | :---: |
| <img src="docs/images/RemoteKnown正在被远程的-桌面提示.png" alt="远程开始" width="400"> | <img src="docs/images/RemoteKnown结束远程-桌面提示.png" alt="远程结束" width="400"> |

### 系统托盘
| 红色告警状态 |
| :---: |
| <img src="docs/images/RemoteKnown正在被远程的-右下角有红色提示.png" alt="托盘告警"> |

### 通知设置
| 飞书设置 | 钉钉设置 |
| :---: | :---: |
| <img src="docs/images/RemoteKnown设置通知-飞书.png" alt="飞书设置" width="400"> | <img src="docs/images/RemoteKnown设置通知-钉钉.png" alt="钉钉设置" width="400"> |

## ✨ 核心功能

*   **🛡️ 实时感知**：多维度信号（进程、窗口、网络端口、Session）综合判定。
*   **👁️ 多工具支持**：
    *   [x] ToDesk
    *   [x] 向日葵 (Sunlogin)
    *   [x] Windows 远程桌面 (RDP)
    *   [x] 网易UU远程
    *   [x] AskLink远程
    *   [x] 远程看看
*   **📝 会话审计**：自动记录每次远程控制的**开始时间**、**结束时间**、**持续时长**及**判定来源**。
*   **🔔 多渠道告警**：
    *   **桌面右下角弹窗通知**
    *   **系统托盘状态变色**（绿色安全，红色警告）
    *   **即时通讯软件推送**（支持飞书 Webhook、钉钉 Webhook）
*   **🔒 隐私优先**：所有数据均存储在本地 SQLite 数据库中，**不上传**任何敏感信息。

## 🚀 快速开始

### 下载安装
请从以下任一平台下载最新的安装包 (`RemoteKnown-Setup-x.x.x.exe`) 并安装：

*   **GitHub**: [Releases 页面](https://github.com/samwafgo/RemoteKnown/releases)
*   **Gitee**: [Releases 页面](https://gitee.com/samwaf/remote-known/releases)
*   **AtomGit**: [Releases 页面](https://atomgit.com/SamWaf/RemoteKnown/releases)

### 运行
安装完成后，双击桌面图标启动。
*   程序启动后会自动最小化到系统托盘。
*   当检测到远程控制时，托盘图标会变红，并弹出提示。
*   点击托盘图标可打开主界面查看详细状态和历史记录。

## 🛠️ 编译构建

如果您是开发者，想要自行构建项目，请遵循以下步骤：

### 环境要求
*   **Windows 10/11** (核心检测逻辑依赖 Windows API)
*   **Go**: 1.21 或更高版本
*   **Node.js**: 18 或更高版本 (推荐使用 LTS) 

### 构建步骤

我们提供了一键构建脚本，自动处理 Go 后端编译和 Electron 前端打包。

**必须以管理员身份运行 CMD 或 PowerShell（解决软链接权限问题）：**

```powershell
# 1. 克隆项目
git clone https://github.com/samwafgo/RemoteKnown.git
cd RemoteKnown

# 2. 运行构建脚本 (会自动安装依赖并打包)
.\build.bat
```

构建完成后，安装包将生成在 `web/dist` 目录下。

## 📂 项目结构

```
RemoteKnown/
├── build.bat             # 一键构建脚本 (Windows)
├── cmd/                  # Go 程序主入口
├── internal/             # Go 核心业务逻辑
│   ├── detector/         # 远程特征检测引擎
│   ├── server/           # 本地 HTTP API 服务
│   └── storage/          # SQLite 数据库操作
├── web/                  # Electron 前端源码
│   ├── assets/           # 静态资源 (Logo等)
│   ├── index.html        # 主页面
│   └── main.js           # Electron 主进程 (含单实例锁、后端守护)
└── README.md             # 项目文档
```

## 🤝 参与贡献

欢迎提交 Issue 和 Pull Request！

### 代码仓库
*   **GitHub**: [https://github.com/samwafgo/RemoteKnown](https://github.com/samwafgo/RemoteKnown)
*   **Gitee**: [https://gitee.com/samwaf/remote-known](https://gitee.com/samwaf/remote-known)
*   **AtomGit**: [https://atomgit.com/SamWaf/RemoteKnown](https://atomgit.com/SamWaf/RemoteKnown)

### 交流沟通
欢迎关注我们的微信公众号，获取最新动态和技术交流：

<div align="center">
  <img src="docs/images/mp_samwaf.png" width="400" alt="微信公众号">
</div>

## 📄 开源协议

本项目采用 [MIT License](LICENSE) 开源。
