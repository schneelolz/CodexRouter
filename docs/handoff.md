# macOS 开发接手与跨平台整合

macOS 实现基于公开基线 `a531544`，与 Windows 共用 Go 路由核心、Vue 管理页、API 1.8 和 SQLite schema 3。通过独立开发提交及普通合并保留两端历史；发布只包含源码、通用文档和脱敏验证摘要。本机安装记录及原始证据不进入公开历史。

- Darwin 使用 CGO / Security.framework / AppKit；其他平台由 build tag 隔离，保留 Windows 凭据与托盘实现。
- Keychain 按数据目录规范路径隔离；普通访问禁止交互，显式本地授权命令才请求系统窗口。凭据空间迁移需要确认并保留原条目，不自动切换线路。
- 菜单栏运行在主线程，`.app` 包含独立 UI、图标与完整 ad-hoc 签名；SIGINT/SIGTERM 走有界流式排空。未提供自启动服务或 Developer ID 公证。
- macOS CLI 管理入口仅做隔离探测；桌面多实例隔离未证明，继续禁用独立桌面启动。CLI 合成协议不代表真实桌面验收。
- 已复现的临时 warm-fork 允许显式父 Session-Id 与子元数据组合，子线程独立绑定；未知父及其他冲突仍拒绝。
- Unix 存储检查私有权限、目录归属及符号链接；日常接入保留非连接字段及恢复依据，不自动改真实配置。

构建、迁移与验收入口见 [macOS 说明](macos.md)，测试边界见 [验证记录](verification.md)。源码同步不部署现有应用、不操作用户凭据或正在运行的客户端。

# 开发说明

## 目录与职责

| 目录 | 职责 |
|---|---|
| `cmd/codexrouter` | 参数、GUI/前台入口、托盘、启动及退出 |
| `internal/router` | 管理 API、固定供应商路由、流式转发、结果与用量观察 |
| `internal/store` | JSON 状态、原子写入与进程锁 |
| `internal/diaglog` | 有界异步JSON日志、轮转、重启恢复与降级计数 |
| `internal/history` | SQLite 请求、分页、策略、待登记及绑定查询投影 |
| `internal/clients` | 独立测试客户端配置、检测与 owned 进程回收 |
| `internal/daily` | 原客户端连接字段预览、备份与恢复 |
| `internal/instance` | 重复启动唤起协议 |
| `internal/platform` | Windows 凭据、托盘、用户目录与平台适配 |
| `internal/api` | OpenAPI 生成的 Go 类型 |
| `frontend` | Vue 页面、合成开发及浏览器测试 |
| `contracts` / `scripts` | 契约、类型生成、构建与检查 |

Go 不嵌入前端。生产静态文件由显式 `ui/` 目录托管，可独立构建；整目录替换应停止对应实例并保留可回退副本，避免新旧资源混合。

## 工具与构建

Go 版本见 `go.mod`，前端依赖见锁文件。开发验证使用 Node.js 24、PowerShell 7、Windows；生产后端构建禁用 CGO。构建脚本优先 `%LOCALAPPDATA%\CodexRouter\toolchains\go\bin\go.exe`，不存在则使用 PATH 中的 Go。

```powershell
.\scripts\build.ps1 -BackendOnly -OutputDir artifacts\backend -Version 0.8.0-test-compact-ui1
.\scripts\build.ps1 -FrontendOnly -OutputDir artifacts\frontend
```

两种开关同时传入会失败。需要前端时已有输出 `ui/` 会拒绝覆盖，应选择新目录。`-SkipInstall` 只在已准备好锁文件对应依赖时使用。源码与构建脚本默认标签属于历史值，正式标识必须显式指定 `-Version`。

## 契约与检查

修改 `contracts/openapi.json` 后运行 `node scripts/generate-contract.mjs`，提交生成的 Go/TypeScript 类型，再运行 `node scripts/contract-check.mjs`。生成器仅支持本契约实际使用的 schema 子集，不是通用 OpenAPI 生成器。

`.\scripts\verify.ps1 -InstallBrowser` 顺序执行契约、vet、Go 普通/race、npm ci、前端类型/单测/构建、mock E2E 和真实 Go API 联调。该脚本当前要求 Windows race 工具链位于 `%LOCALAPPDATA%\CodexRouter\toolchains\llvm-mingw-20261020-ucrt-x86_64\bin`，并仅在测试进程设置 CGO/CC。准备该工具链或分别执行可用检查；缺失不能算 race 通过。

UI 测试默认使用 18789，可设置 `CODEXROUTER_UI_TEST_PORT`；真实 Go 联调默认 19962，可设置 `CODEXROUTER_E2E_PORT`。测试端口必须空闲，不复用或停止已有服务。测试数据与依赖输出应位于忽略目录。

实际 Codex CLI 集成默认跳过，显式设置 `CODEXROUTER_CODEX_INTEGRATION=1` 后运行 `TestIsolatedCodexCLIRoutesNewResumeForkAndRestart`。需要同版本 CLI，使用临时目录、合成认证和模拟上游，不代表真实账号验收。`node scripts/protocol-probe.mjs` 是客户端协议探针，`scripts/release-smoke.ps1 -Executable <构建路径>` 与 `scripts/drain-smoke.mjs --exe <构建路径>` 是独立进程烟测。

## 存储和维护

JSON 绑定是路由权威，SQLite 会话索引可从其修复；待登记与请求历史不能仅靠绑定重建。历史失败会显示降级；启动时损坏或未知 schema 拒绝启动，不重建真实库。保留策略不删除路由绑定。

日常配置只改连接字段，保留其他字节；外部编辑和无法识别的恢复状态会拒绝覆盖。凭据与 JSON 跨存储的补偿不等于跨进程崩溃事务。不要自动接管日常客户端或通过编辑真实状态改线。

此发布副本未携带依赖私人验收材料的旧打包脚本、旧开发历史或本机交接。源码构建与测试使用上方现有入口；创建二进制发行需另做产物、许可、文档、摘要和隐私复核。

## 持续集成

`.github/workflows/ci.yml`使用Windows runner、go.mod指定Go版本和Node.js24，分别执行Go普通/race及启动验收、前端契约/类型/单测/构建/浏览器/Go联调。Actions固定到提交SHA，只读仓库权限；浏览器证据仅来自合成夹具，保留7天。CI不启动真实Codex或使用真实账号。

页面层级、滚动和协议结果/传输收尾区分见[页面与指标](ui-layout.md)。接口1.8和schema3升级边界见[契约](../contracts/README.md)。
