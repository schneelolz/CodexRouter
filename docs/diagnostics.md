# 故障诊断日志与管理认证恢复

本轮候选版本为 `0.9.0-test-diagnostics-auth1`。管理 API 为1.8，历史数据库仍为schema3。验收范围是日志与认证恢复，生成断流的真实根因留待故障现场关联确认。

## 日志位置与保存方式

默认日志在 `%APPDATA%\CodexRouter\logs\router.log`，指定 `--data-dir` 时位于对应数据目录的 `logs` 子目录。内容为 UTF-8 JSON Lines，每行一个事件。

每个文件最多10 MiB，包含当前文件共保留5份，旧文件依次为 `router.1.log` 至 `router.4.log`，数字越大越旧。超过保存容量时轮转。日志独立于请求历史，页面清理请求历史不清理这些文件。

写盘使用容量1024的内存队列，投递不等待磁盘。每秒执行一次磁盘同步，正常退出先排空队列再同步。队列满时丢弃日志并计数，目录或文件不可写时服务继续运行，后续事件会尝试恢复写盘。恢复写盘不会清零计数。单条记录超过16 KiB也计入缺失。

现有近期诊断接口会报告 `LOG_DROPPED` 和 `LOG_WRITE_FAILED`。落盘记录的 `log_dropped`、`log_write_failures` 为当前进程累计数量，零值省略。强制结束、系统崩溃或断电可能损失队列和未同步记录。重新启动会清除当前文件没有换行符的末尾半行并计数，之前完整行继续保留。

## 事件与字段

每行包含 `format_version=1`、UTC RFC3339Nano时间、`program_version`、随机 `run_id`、`event` 和结构化 `fields`。同一启动内通过 `request_id` 关联，跨启动用 `run_id + request_id`。`history_id` 对应已有 SQLite 请求记录，未能登记时为0。

| 事件 | 记录内容 |
|---|---|
| `process_start`、`process_ready`、`process_stop` | 启动、服务就绪、退出及正常或失败分类 |
| `management_open`、`management_open_result` | 本机打开管理页动作及浏览器启动结果 |
| `bootstrap_issued`、`bootstrap_exchanged`、`session_replaced` | 引导签发、成功交换与管理会话替换 |
| `session_renew_checked`、`session_renewed` | 认证续期检查与实际延期 |
| `auth_rejected`、`request_rejected` | 固定认证或来源拒绝原因 |
| `request_start` | 本机请求编号、历史编号、固定入口、当前超时配置 |
| `upstream_headers` | 上游实际HTTP状态、HTTP版本、编码、有效的上游请求ID及连接测量 |
| `upstream_request_failed` | 未取得最终响应头时的固定网络错误分类 |
| `stream_read_failed` | 上游读取失败分类以及本地超时、取消或退出原因 |
| `request_end` | 最终结果、来源、协议终止、耗时、字节数及首末收包时间 |
| `http_server_error` | 固定的HTTP宿主错误或处理器异常分类 |

连接测量包含 `dns_ms`、`connect_ms`、`tls_ms`、`send_ms`、`header_wait_ms` 及对应结果。`reused`、`was_idle`、`connection_idle_ms` 描述连接复用。没有发生或不能测量的阶段为 `null`／`unknown`，例如复用连接通常没有本次TCP、TLS耗时。并行IPv4/IPv6拨号保留成功连接的耗时，失败尝试不会覆盖成功结果。

`request_body_bytes` 表示HTTP传输层从请求正文读取的字节数。`response_read_bytes` 是上游响应正文读取量，`response_written_bytes` 是下游Writer接受的正文量，包含压缩时的原始压缩字节。计数不包含HTTP头，Writer接受不等于客户端已经完整处理。

`first_body_at`、`last_body_at` 是正文读取时间，`last_read_idle_ms` 是结束时距最后一次正文到达的间隔。`terminal_result` 是实际观察到的Responses终止结果。`result_source` 标识最终结果来源，`transport_source` 与 `transport_result` 单独描述连接收尾，例如收到完成事件后客户端取消连接，两者均保留。`upstream_error_type` 仅保留允许的分类，例如 `server_error`、`rate_limit_error`，其他值为 `unknown`。

`upstream_request_id` 仅在响应头提供且满足1～128字符的字母、数字、点、下划线、冒号或短横线规则时保存。上游未提供有效ID时该字段缺失，需结合供应商与时间对照其服务日志。

认证拒绝原因包括 `cookie_missing`、`session_expired`、`cookie_mismatch`、`csrf_mismatch`、`origin_mismatch`、`bootstrap_expired` 和 `bootstrap_invalid_or_consumed`。认证事件不保存令牌或其摘要。

## 故障时怎样判断

1. 记录失败时间，在 `request_end` 查找对应 `history_id`，再按 `run_id + request_id` 找齐该次事件。
2. `UPSTREAM_TIMEOUT` 配合连接阶段结果判断DNS、建连、TLS或等待响应头。检查是否复用连接，以及超时当时的配置。
3. `STREAM_IDLE_TIMEOUT` 表示本地上游读取等待达到限制。结合 `last_body_at`、上游编码、终止结果和上游请求ID核对静默发生位置。
4. `upstream_status=502` 或协议 `RESPONSE_FAILED` 表示已经收到上游失败响应。用 `upstream_request_id` 到sub2api核对账号调度、上游或代理日志。
5. `CLIENT_WRITE_FAILED`、`REQUEST_CANCELED`、`ROUTER_SHUTDOWN` 分别定位客户端写入、取消和服务关闭。仅凭本机网络错误分类无法确定远端内部根因。
6. 同时查看日志缺失计数。缺少结束记录但存在开始记录时，结合进程退出时间与请求历史中的进程中断记录判断是否被强制终止。

全部字段由调用方选择的白名单产生。日志不保存请求或响应正文、Cookie、Authorization、引导令牌、CSRF、完整URL、任意响应头及未经筛选的原始错误文本。HTTP宿主原始错误也只转换为固定类别。诊断文件仍包含请求时间、供应商标识和关联编号，应按本机运行资料保存。

## 管理页面恢复

页面监听新引导链接的 `hashchange`，立即移除地址中的令牌并重新认证。重建请求作用域时会取消前一轮请求，迟到的连接、轮询和续期结果不会覆盖当前状态，未保存的设置草稿继续保留。请求历史子组件随认证作用域重建，取消旧列表请求。

多个标签共享Cookie，但各自保存内存CSRF。续期收到明确 `403 CSRF_REQUIRED` 时，读取当前有效会话的CSRF并最多重试一次续期。真正失效的Cookie返回401，仍需从托盘重新引导。普通保存、删除等业务写操作不会因此自动重试。

托盘仅处理双击打开动作，右键菜单打开继续可用，避免一次鼠标双击中的释放事件重复签发引导。

## 运行与切换

测试包包含配套的 `codexrouter.exe` 和 `ui` 目录。双击使用默认数据目录，已有实例持锁时会唤起已有实例。启用新程序需要从独立控制入口安排退出旧实例、整体备份数据目录，再使用配套程序与界面启动。当前对话通过现用路由器连接，切换安排由独立入口执行。

浏览器Cookie按主机和路径隔离，端口不同仍会共享同名Cookie。并行手动验证多个实例时，需要分别使用浏览器配置，并确保各自引导链接进入对应配置。自动化验收已使用独立浏览器上下文。独立数据目录的供应商配置与系统凭据需要单独设置。

## 验证记录

2026-11-07 功能工作区完成Go测试、vet与race、24项前端单测、22项浏览器回归和2项真实Go后端联调。统一脚本前9项通过，最后的联调通过独立补验。测试中的等待顺序和运行预算修正已包含在本次源码中。

30分钟隔离持续测试通过，共1639次请求、4985条日志，日志缺失和写盘失败均为0。结束时活动请求和上游连接归零，Go堆1222200字节、协程6。实际Windows EXE启动与日志冒烟6项通过，诊断专项10次race重复与托盘生命周期检查通过。

测试只使用合成数据。原始运行日志、截图和协议数据在本地保存，仓库提供脱敏的 [验证摘要](verification-results.json)。真实上游断流根因需要部署新版本后用时间及上游请求ID关联确认。GitHub CI结果以提交对应的Actions运行结果为准。
