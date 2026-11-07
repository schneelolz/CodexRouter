# macOS 开发与验收

本分支基于 `a531544` 的 Go 后端 / Vue 前端，保留 API 1.8、SQLite schema 3 及 Windows 实现。不是早期 TypeScript 后端方案。

## 能力边界

| 范围 | 当前状态 |
|---|---|
| Apple Silicon 前台 HTTP/SSE、管理页面、Keychain | 本机原生自动化已验证 |
| 菜单栏打开管理页 / 运行状态 / 退出 | 主线程/信号/原生菜单动作分发、重复 launch 真实系统浏览器唤起与认证已验证；外部鼠标点击和外观待验收 |
| `.app` / ZIP / Intel | 提供构建入口；Intel 仅交叉构建，未运行 |
| CLI 发现与独立检查 | 本机发现 CLI 0.166.0；管理页提供独立 `--version` 和专用目录登录状态检查，不启动交互代理、不代为登录 |
| CLI 协议 | 0.166.0 在独立 HOME / CODEX_HOME、合成认证与合成上游完成 new / resume / fork / 重载绑定验证 |
| 桌面发现 | 本机 `/Applications/ChatGPT.app`，bundle ID `com.openai.codex`，26.1021.31345；内置 CLI 0.164.0-alpha.11.1 的普通/临时侧边 fork 八轮隔离协议已验证；完整 UI 实测尚未完成 |
| 桌面隔离启动 | **未验证，后端拒绝、界面禁用**；未启动真实桌面或复用日常 app-server |
| 日常预览、确认、备份、冲突与恢复 | 临时夹具和 Go API 通过，真实接入/恢复及账号完整验收尚未完成 |

这是可运行的 macOS 开发候选，不是已完成真实账号/桌面验收的正式发布。

## 后台运行与认证

应用以菜单栏方式长期运行，关闭管理网页不会停止转发。退出菜单栏应用、注销/重启系统或系统休眠会停止/暂停服务；尚未设置登录自启，也没有安装常驻服务。重启后须手动启动应用，直到未来提供明确选择的登录项。路由器未运行时不会自动改回官方直连。

管理网页会话有效期 8 小时，可见页面每 5 分钟检查续期；后台标签或关闭网页可使管理认证过期，从菜单栏重新打开即可。此会话不用于推理转发，已用合成回归验证过期时两条线路仍转发成功。

官方身份由 Codex 客户端持有并刷新，路由器不代为管理 refresh token；若官方登录真的失效仍需在 Codex 中重新登录。API Key 在 Keychain 持久保存，但密钥撤销/过期/余额不足或 Keychain 锁定/拒绝访问仍可能导致请求失败。程序不会自动切换供应商或重放生成。尚未做多天真实网络长稳验收。

如果“用量明细”没有数据，先看“待确认”和“错误请求”：HTTP 200 不代表观察到了 response.completed，用量和格式无法可靠解析时必须保留待确认。未知响应格式保持待确认，不能据此宣布桌面完整兼容。

## 凭据空间与更新

普通启动不弹出钥匙串授权窗口。`keychain-check` 只输出固定状态；`keychain-authorize` 是显式本地命令，由用户在系统窗口决定是否授权，不输出密钥、不修改全局 ACL。不同 ad-hoc 构建可能需要重新授权，完整包签名不等于已有条目已授权。

显式 `keychain-profile` 提供 preview、prepare、check、activate、rollback。prepare --confirm 建立待激活空间及新的管理凭据；供应商密钥由用户重新录入。activate --confirm / rollback --confirm 必须停止目标实例并取得其锁。选择记录须私有且非链接；损坏时失败，不静默回退。原条目、路由状态、绑定、历史与日常恢复依据保持，程序不复制或解密旧密钥。

更新前正常停止目标实例并备份完整应用与数据目录。回退需使用配套应用和选择记录，不能把任意旧程序当作兼容版本。新凭据空间不恢复系统钥匙串密码，也不等于已验证注销/重启后的解锁，详见 [Keychain 更新与恢复](keychain-recovery.md)。

## 依赖与构建

需要 macOS、Apple Command Line Tools（`xcode-select --install` 由用户自行决定）、`go.mod` 指定的 Go 1.27.3、Node.js 和 npm。CI 固定 Node 24。不要降低 Go 版本；构建脚本使用 `GO_EXE`，否则优先项目 `artifacts/toolchains/go/bin/go`，最后使用 PATH 中的 Go。

macOS 使用 CGO 和 Security / Foundation / AppKit 系统框架，以及 CLT 自带 Swift/AppKit 生成原生 ICNS 图标，不能沿用 Windows 的 CGO=0 发布参数。产物声明最低 macOS 13.0；未在最低版本实机验收。Go 工具链只安装在本项目忽略目录，未升级全局工具。

```bash
cd CodexRouter
bash scripts/build-macos.sh --out artifacts/macos-local --version 0.8.0-macos-dev
# 已执行 npm ci 时可加 --skip-install
# Intel 交叉构建，不能当作 Intel 实机验证：
bash scripts/build-macos.sh --arch amd64 --out artifacts/macos-intel-local --version 0.8.0-macos-dev --skip-install
```

输出目录必须不存在，防止混合旧 UI。目录包含 `CodexRouter.app`、指向包内程序的 `codexrouter`、架构对应 ZIP 和 `build.json`。仅打包编译程序、静态页面和许可，不打包配置、认证或运行数据。不安装到 `/Applications`，不安装常驻服务。脚本在资源完整后为整个 `.app` 做本地 ad-hoc 签名，并对ZIP解包内容严格验签；仅依赖Go链接器的签名不足以保护应用包资源。此处仍不代表Developer ID签名、公证或Gatekeeper验收。不要移除 quarantine、禁用 Gatekeeper 或把下载发行包的行为当成本机源码构建验证。

## 启动与退出

下面选独立数据目录，不会自动接入日常 Codex。端口只是示例，若占用则选择另一个空闲端口，不终止其他程序。

```bash
# 前台，保留完整 .app 目录：
./artifacts/macos-local/codexrouter serve \
  --ui-dir "$PWD/artifacts/macos-local/CodexRouter.app/Contents/Resources/ui" \
  --data-dir "$PWD/runtime/macos-trial" --port 19973
# 前台终端输入 open 打开管理页；输入 quit 或按 Ctrl-C 正常退出。

# 菜单栏；会打开管理页，只创建本实例数据，不修改 Codex：
./artifacts/macos-local/codexrouter launch \
  --data-dir "$PWD/runtime/macos-trial" --port 19973
# 不自动打开浏览器：在上述命令加 --open=false
# 退出：菜单栏 Quit CodexRouter，或当前前台终端 Ctrl-C。
```

也可 `open artifacts/macos-local/CodexRouter.app`，这会使用默认 `~/Library/Application Support/CodexRouter` 和 18788；此入口使用默认数据目录，验证时优先选择上方隔离命令。重复 `launch` 会认证后唤起同一数据目录的已有实例；持锁拒绝、真实系统浏览器唤起/页面认证和原生菜单动作分发已验收，外部鼠标点击和 Finder 双击仍未验收。

不要按名称批量终止进程。退出只回收本实例持有句柄的只读测试子进程，最多排空活动请求 5 秒；退出路由器**不会恢复**已确认的日常接入。

## Keychain 与文件保护

- 生产使用当前用户默认钥匙串，service 为 `CodexRouter/<数据目录规范路径 SHA-256>`，account 为受限凭据名称；密钥通过内存中的 Security.framework API 传递，不进入进程参数、文件或日志。
- 使用 Darwin `F_GETPATH` 获取已存在目录的实际拼写，解析系统/目录别名；不存在的后缀保持大小写，不做 Windows 式小写折叠。重命名/移动数据目录会改变命名空间，复制目录不会迁移密钥。
- 普通启动与转发禁止 Keychain 自动交互；显式本地 keychain-authorize 命令才临时请求系统授权，结束后恢复禁止交互，不向浏览器暴露授权入口。锁定、拒绝、无默认钥匙串或应用 ACL 不允许访问时返回固定 OSStatus，不明文回退。由用户决定系统授权，不自动解锁、删除条目、重置密钥或修改全局 ACL。不同 ad-hoc 构建通常需要重新授权。
- Unix 数据目录必须属于当前用户且不向组/其他用户开放；新目录 0700，JSON/实例锁 0600，主进程 umask 0077。不会自动修改既有目录权限；权限错误时选新的私有目录。
- 日常配置拒绝用户符号链接，只允许固定的 macOS 系统 `/var`、`/tmp`、`/etc` 别名；原配置权限及非连接字段保持。原子替换后同步目录，继续使用既有恢复日志。非协作外部程序的精确检查/替换窗口和物理断电不是已解决的文件系统事务。
- Keychain 单测使用临时钥匙串，比较创建期间及删除后的用户搜索列表；不更换默认钥匙串。独立烟测只创建临时数据目录命名空间的合成 `control` 条目，结束时定点删除并验证不存在。

## 自动化

```bash
bash scripts/verify-macos.sh --install-browser
# 可选：额外启动本机 CLI 0.166.0 的受限合成协议测试；不使用真实账号：
bash scripts/verify-macos.sh --with-cli-probe
# 单独检查构建：
node scripts/macos-smoke.mjs --exe artifacts/macos-local/codexrouter
node scripts/drain-smoke.mjs --exe artifacts/macos-local/codexrouter
# 只读检查真实日常 config.toml；不读取认证/历史，不创建恢复目录：
CODEXROUTER_DAILY_READONLY=1 ./artifacts/toolchains/go/bin/go test ./internal/daily \
  -run '^TestDarwinDailyReadOnlyPreflight$' -count=1 -v -timeout 30s
# 显式交互验收，会在系统浏览器新开测试页；不关闭用户浏览器：
node scripts/macos-interactive-smoke.mjs --exe artifacts/macos-local/codexrouter --open-system-browser
```

交互脚本仅使用临时 CODEX_HOME/数据目录，路由器保留正常 HOME 以访问系统 Keychain，CLI 子进程独立 HOME。桥接使用后端唯一允许的开发源 `127.0.0.1:18789`，必须空闲，拒绝接管已有服务；普通套件端口仍动态选择。脚本不记录认证/正文，测试标签页可能留在系统浏览器中。缺少辅助功能权限时不注入鼠标事件，不将菜单动作分发等同实际鼠标点击。

统一入口运行契约、模块校验、vet、普通/race、前端类型/单测/构建、mock 浏览器、真实 Go API + 合成上游、独立程序和排空检查。浏览器端口动态选择，拒绝复用已有服务。脚本输出和截图位于忽略目录；平台条件跳过必须单列，不能计作通过。新增 macOS CI 不启动真实 Codex，Windows CI 保持原样；本地交叉编译不代表 CI 或 Windows 运行通过。

## 后续人工验收

以下步骤**未自动执行**。先保存工作，逐步确认，不能因为 CLI 合成测试通过就推断桌面通过。

1. 菜单栏：用独立数据目录、空闲端口 `launch`，点击 Open Management、观察状态、点击 Quit。只影响本次路由器/浏览器，不影响当前 Codex。
2. 专用 CLI 真实账号：新建另一个私有测试目录，明确 `CODEX_HOME`、HOME、空白工作目录、文件凭据存储与禁用工具配置；由本人登录。会产生新的认证/网络请求，不应修改日常配置；本候选不提供自动登录或交互代理启动按钮。先复核对应 CLI 版本的隔离参数，不能复制真实 auth.json。当前不宣称此项通过。
3. 日常预检：可显式运行只读预检，核对配置格式及覆盖来源。管理页预览不修改 config.toml，但可能创建 `.codexrouter-routing` 与实例锁，所以不应把它视为完全零写入。预览前核对固定配置路径，不输出或分享配置正文；CLI 与桌面可能共享该目录。
4. 确认接入：用户批准后才应用；会改真实配置中的 `openai_base_url` 并在同目录 `.codexrouter-routing` 保留恢复依据，不修改认证、历史、MCP 和其他字节。正在运行的会话不会被本程序终止，但共享后台可能仍持有旧地址。
5. 真实 CLI：由用户决定何时退出/重开相关 CLI 或共享后台，再新建受控会话核对线路与绑定；这一步**可能中断现有任务**。使用未接入的旧进程不能证明切换成功；不要自动重放生成。
6. 真实桌面：保持“独立测试桌面”禁用。只有用户决定关闭现有客户端后，才能逐项核查 app/bundle/内置 CLI 版本、独立实例与数据写入位置、是否复用旧进程和配置实际生效。此步**明确影响现有桌面会话**，需另行批准；环境变量/参数本身不是隔离证据。
7. 恢复：管理页预览并确认“恢复接入前配置”，还原的是备份连接值，不一定是官方默认；由用户在合适时间重开相关客户端，验证实际连接后再停止路由器。需要重开的步骤会影响运行中任务。

## 故障与回退

未接入日常配置时，只需正常退出本实例；本次测试已定点清理合成凭据，不应清理整个钥匙串。端口冲突选择新端口；数据目录持锁不要删锁或终止不明进程。

已接入时保留原数据目录和 `.codexrouter-routing`，先通过预览/确认恢复；冲突或恢复日志异常时停止写入、保留现场，不删除恢复记录、不猜原值。切换旧版本前正常停止目标实例并整体备份数据目录，schema 3 回滚需要匹配的升级前副本。移动目录导致 Keychain 命名空间改变时，优先恢复原路径，而非遍历/复制真实凭据。
