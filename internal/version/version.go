// Package version 提供守护进程（exe）的版本号，作为检测规则 minAppVersion 门槛的当前版本基准。
package version

// Version 是守护进程的版本号，与前端 Electron 版本（web/package.json）保持一致。
//
// 构建时通过 ldflags 注入覆盖此默认值，无需手工改这里：
//   - 本地 build.bat / Makefile：从 web/package.json 读取 version 注入
//   - GitHub 发布（.github/workflows/release.yml）：从 git tag（去掉前缀 v）注入
//     例如打 tag v1.0.6 → 注入 Version=1.0.6
//
// 注入命令形如：
//
//	go build -ldflags "-X RemoteKnown/internal/version.Version=1.0.6" ...
//
// 因此发布新版本时只需：① 改 web/package.json 的 version；② 打同名 git tag。
// 这里的默认值仅用于未注入的本地 `go run` / IDE 构建，需大致与 package.json 保持一致。
var Version = "1.0.5"
