# 管理前端

本工程独立于 Go，使用 Vue 3、TypeScript、Vite 与 Naive UI。生产页面只通过同源 `/api/v1/` 通信，没有 CDN、运行时配置注入或浏览器持久凭据。

```powershell
npm ci
npm run dev:mock   # 仅开发模式载入合成 API；无需 Go
npm run dev        # /api/ 代理至 127.0.0.1:18788
npm run typecheck
npm run test
npm run build
npx playwright install chromium
npm run test:e2e
npm run test:integration  # 先 build，再通过测试专用 Go fixture 联调生产静态目录
```

Vite 固定监听 `127.0.0.1:18789`，端口冲突直接失败。开发代理将 Host 改为后端的精确监听地址，同时保留浏览器 Origin；后端须通过 `--dev-origin http://127.0.0.1:18789` 显式允许开发来源。Origin、认证与 CSRF 检查仍然生效。

`dist/` 是可独立替换的发布产物。将整个目录装配到 Go 发布目录的 `ui/`，由 `serve --ui-dir ./ui` 同源提供；也可以使用其他静态宿主，但必须同源反向代理 `/api/`、保留 Host/Origin，并使用后端允许的 loopback 监听地址。

生产构建不载入 mock。`dev:mock` 显示“合成演示数据”，不能证明真实上游或 Codex 兼容性。E2E 使用合成 HTTP API 测试错误、首次登记、供应商增删、密钥状态、键盘和响应式布局，截图保存在 `test-results/`。

`test:integration` 在 `127.0.0.1:18788` 启动 Go 测试专用服务，使用合成内存凭据和状态，验证真实认证 cookie、CSRF、配置与供应商写入、按供应商的密钥状态与生产静态托管；不接入真实上游，不访问日常 Codex 配置。此测试需要 Go，浏览器不保存认证 trace。该端口必须空闲，已有服务不会被复用或终止。

视觉采用蓝灰侧栏、白色操作区；供应商卡片与默认供应商选择为主要操作，橙色只用于待登记。每个供应商卡片分别维护名称、基址、模型覆盖和密钥，已绑定供应商的基址显示为锁定。字体使用本机 Segoe UI Variable / Microsoft YaHei UI，不请求外部字体。界面将上游显示为“最近成功 / 最近失败 / 未验证”，断连时明确标记旧数据，禁止配置写入。

## 页面与滚动约定

`navigation.ts`统一页面标题与导航，`AppNavigation.vue`处理桌面展开及手机入口；`SessionTable.vue`只展示一种会话列表，页码、每页数量和供应商选择由App保留。请求明细为八列、64px普通行高。`page-main`固定标题，`page-content`承载表单滚动，列表使用`data-viewport`独立滚动并固定表头／分页。时间阈值与说明共享`request-format.ts`中的`latencyThresholds`。

E2E包含61条合成数据的逐页三视口检查、长字段、复制、子菜单状态及行高测量，结果在`test-results/final-layout-metrics.json`；代表截图以`final-`开头。完整交付检查清单见`../docs/ui-layout.md`。
