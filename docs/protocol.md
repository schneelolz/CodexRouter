> 当前接口补充：API1.8、schema3；续期、页码快照、known_fields及协议/传输独立结果以[管理契约](../contracts/README.md)为准，旧记录不补造指标。

# 协议与路由边界

## 范围

推理入口为 `POST /responses`、`POST /responses/compact` 和 `GET /models`。只实现 HTTP/SSE，WebSocket 握手返回 426；部分 CLI 0.166.0 测试观察到客户端自行改用 SSE，不能承诺所有桌面或客户端都回退。`/models` 不依靠默认供应商猜测来源，固定查询内置官方供应商。

本项目不是所有路径与请求头均不变的通用透明代理。支持的字段采用白名单，排除逐跳头及 `Connection` 指定的头。管理 API 与推理 API 有各自规则；未知接口不自动发往上游。

## 身份与绑定

`Thread-Id` 必须合法；可用 `Session-Id` 与 `X-Codex-Turn-Metadata` 提供校验。已知 metadata 字段为 `thread_id`、`session_id`、`history_initialization`、`forked_from_thread_id`，大小上限 8192 字节。重复头、已知字段重复、null/错误类型、大小写别名、非法或自父关系均拒绝；未知 metadata 字段可保留兼容。

当前仍要求提供的 thread/session 一致，不推断不同 ID 的会话树关系；已知子代理身份冲突尚未解决。会话绑定键始终为子线程 thread ID，不使用父线程或 session root 替代。

已有绑定优先。未知线程只有 `new` 且正文有可靠无历史的新建证据才分配当前默认供应商；assistant/工具输出、`previous_response_id` 等历史证据会阻止自动新建。`cold_resume` 或未知父来源不能使用默认供应商。

`cold_fork` / `warm_fork` 仅在明确父线程已绑定时继承，子线程独立落盘；落盘成功后才发布内存绑定和转发。未知 fork 类型、`ephemeral_fork` 仍不启用。warm fork 来自独立 app-server 的明确父子关系观察，不能等同真实桌面侧边 UI 或子代理已验证。

## 正文与身份隔离

首次新建证据和模型覆盖有界读取正文，原始及解压输出各限 16 MiB，支持 identity / 单层 zstd。首次证据检查后转发原始字节与编码，已知正文长度保持；后续未覆盖模型的正文按流式发送。

供应商设置非空模型覆盖时，JSON 顶层 `model` 会被替换，其余字段值保留；重新编码可能改变键顺序与重复键，输出未压缩 JSON 和精确长度。未知/叠加编码、非法正文或超限返回错误，不转发。

官方供应商可转发白名单内客户端身份与协议字段，如 User-Agent、请求/window ID，并保留多值 `X-Codex-Turn-State` 响应头。API Key 供应商只用自身密钥，不携带官方 Cookie、账号、thread/session/window 等身份头。

上游重定向不自动跟随；路由器不做生成重放、默认回退或线路迁移。客户端重试单独计数，不能把一个客户端 turn 当作一次物理请求。响应头、空闲读、客户端写与连接超时各自生效；取消会停止相应上游请求。退出最多排空 5 秒。

## 结果与用量观察

转发旁路观察 SSE/JSON，支持 identity/gzip/zstd 统计；保持原始响应字节。单事件/JSON 观察上限 1 MiB，解压窗口 16 MiB，累计观察最多 64 MiB；不支持或超限只降级观察，不以统计需要截断转发。

HTTP 状态和语义结果分开：HTTP 200 的 failed/incomplete 等不视为成功；可可靠解析的 SSE 缺少终止事件归错误 `MISSING_TERMINAL`；解析不可靠、格式不支持或观察超限时归待确认。成功仅指该次生成协议观察完成，不代表用户整个任务完成。`known_fields` 区分上游报告的零和未报告；重复 usage 快照不累计，输入含缓存，推理为输出子集。

只记录会话标识、模型、状态、固定错误码和用量等元数据，不持久化聊天正文、认证或原始上游错误正文。诊断仍可能涉及私人会话关系，分享前需脱敏。真实账号、桌面、compact 及第三方网关兼容性见[验收状态](acceptance-matrix.md)。
