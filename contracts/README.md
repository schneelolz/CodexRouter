## 1.8 协议结果与连接收尾分离

RequestActivity新增`observation`，包含`terminal_result`（未确认时unknown）、`transport_result`（EOF或固定传输类别）、`metrics_status`（available/partial/not_reported/unavailable/not_applicable/legacy/pending）。数字仍必须由metrics.known_fields逐项确认，状态不使未知数值变成0。

成功终止事件在写入及Flush成功后被完整观察到，随后客户端取消只作为传输收尾保存，不覆盖生成完成。实际写入/Flush失败、观察丢失、中途取消、上游错误或路由器关闭仍不得升级为成功；明确协议失败与其后的传输取消独立保存。

SQLite schema3只新增observation JSON列，旧记录的结果和metrics不重写；缺少新元数据时标记legacy，GET /models为not_applicable。处理中的记录为pending。前端兼容API1.7读取，缺少新增字段时仅作旧记录说明；新构建后端API1.8提供完整收尾事实。

schema3不能由旧二进制打开。替换实例前正常停止并整体备份数据目录，回退时使用升级前完整数据副本，不单独删除SQLite附属文件。

## 1.7 续期、页码与协议完成时间

新增POST `/api/v1/auth/renew`：同源Origin、有效Cookie与CSRF齐备，剩余不足1小时延长至8小时并刷新同令牌Cookie；不允许过期会话恢复，不轮换CSRF。读取不会续期。

请求查询支持`page`及`snapshot`，新增响应`page/total_pages/snapshot`。page为1～1000000、与cursor互斥；snapshot只允许page模式，绑定filter、limit及首次最大ID。仅限制新插入上界，状态变化和清理仍实时可见；越界页校准。旧游标模式保留，新UI要求API1.7。

RequestMetrics新增`protocol_completed_ms`；存在known_fields中才可用。`elapsed_ms`保留完整连接结束时间；有明确终止事件时列表总耗时和TPS使用协议结束时间，详情并列连接时间。schema2不变，旧记录缺少指标时不补造。

非缓存输入仅在input/cache_read/cache_write全部known且桶一致时相减计算；存储与DTO中的input_tokens仍为含缓存总量。见[本轮说明](../docs/界面与续期验收-20261106.md)。

## 1.6 请求结果与用量

RequestActivity新增outcome、model、request_type、error_stage和metrics。metrics所有数字字段始终存在，但只有known_fields中列出的字段可视为真实报告（包括0）；其余显示未报告。输入保存Responses总输入，缓存为输入子集，推理为输出子集；total_tokens不重复加缓存。first_token_ms只记录流式非空输出delta，不把HTTP响应头或created当首字。

requests新增success/unconfirmed筛选，新UI使用success/error/processing/unconfirmed四类；旧all/normal保留。normal仍兼容HTTP2xx正常结束，不能作为生成成功依据。语义错误保留HTTP原始状态，因此HTTP200也可为error。API版本为1.6，配套UI拒绝旧于1.6的后端。

SQLite schema2事务迁移保留旧记录，旧HTTP200归待确认。回滚需使用升级前完整数据目录副本，旧版不兼容schema2。详见[对照审查](../docs/sub2api-对照审查-20261106.md)。

## 管理API 1.5 增量

- `GET /api/v1/requests?limit=20&cursor=&filter=all`：SQL游标页，filter为all/processing/normal/error，limit为20/50/100。normal仅表示HTTP正常返回，不是SSE语义完成。
- `GET /api/v1/sessions/page?kind=bound&limit=20&cursor=`：kind为bound/pending，分别返回分页绑定/待登记；不删除路由绑定。
- `GET/PUT /api/v1/history/policy`：`{ "days": 0 }`表示不限期，或1～3650天；缺失/null/非整数拒绝，保存后立即执行清理。
- `POST /api/v1/history/clear`：必须`{ "confirm": true }`，返回deleted；仅操作开始前已结束请求，活动请求及会话保留。
- RequestActivity增加status和elapsed_ms；Activity及RequestPage的degraded表示本轮存在过存储故障。旧activity/sessions读取接口保留用于兼容；新界面使用分页。

查询仅上述两个GET入口接受白名单参数，写入继续要求认证、精确Origin及CSRF。非法游标/页大小为400，历史存储故障为503；会话索引从JSON修复失败不会改变绑定。

# 管理接口契约

`openapi.json` 是 OpenAPI 3.0.3 规范，管理接口固定在 `/api/v1/`，当前版本 1.4.0。推理正文不受此规范转换；供应商模型覆盖改写正文的规则见 `docs/protocol.md`。

供应商是独立资源：`/providers` 维护名称、认证方式、基址与模型覆盖，`/credentials/{providerId}` 维护该供应商的密钥（供应商标识同时是系统凭据名称）。新建供应商只接受 `ProviderCreateKind=api_key`；`official` 仅保留给内置官方供应商，避免把客户端官方令牌发往自定义地址。已绑定供应商必须保留非空基址，v1 迁移与 v2 启动校验都会拒绝缺基址的绑定。配置接口只保存默认供应商、超时与并发，不再包含供应商基址，因此已有供应商的资源不会被整体覆盖。页面必须按 `api_version` 次版本判断兼容性。

执行 `node scripts/generate-contract.mjs`，同时生成 Go DTO 和 TypeScript 类型；执行 `node scripts/generate-contract.mjs --check` 检查生成结果。生成器只支持本契约使用的必填对象、引用、数组、枚举及简单标量，遇到不支持的 schema 会失败，不会静默降级。

页面通过 `POST /providers/save` 一次保存名称、基址、默认模型和密钥；新建时 `provider_id` 为空，编辑留空密钥保持原值。修改基址必须重新录入密钥；旧 `PUT /providers/{providerId}` 也拒绝携带已存密钥改址。新增 `POST /providers/models` 支持未保存草稿查询：只使用显式密钥，或同供应商同基址的已存密钥；不接受官方供应商、不传官方身份、不跟随重定向，不默认加 `/v1`。列表接口最长6秒、并发2条、1MiB响应与1000个模型ID上限，只回传去重排序的ID。模型列表成功不写生成成功状态。

统一保存采用状态锁与凭据快照进行失败补偿；凭据与状态文件分属两个存储，不能保证跨进程崩溃的事务性。补偿失败有独立错误码，需要重新录入对应凭据，不宣称原状态完全恢复。

新增日常接口 `GET /daily`、`POST /daily/preview`（固定route/restore动作）和 `POST /daily/apply`（5分钟单次token），只管理固定原配置的openai_base_url字段；不接受文件路径、不读认证历史。预览后配置变化、异实例管理、恢复字段变化、损坏记录均拒绝覆盖。日常状态只能说明配置，不证明客户端重载或实际请求。

`GET /activity` 返回本进程累计入口数及最近100条脱敏请求时间/入口/线程/供应商/HTTP结果；不返回正文或认证，不承诺识别具体客户端窗口，不把HTTP200当生成完成。新增接口沿用同一认证/CSRF/Origin规则。

所有管理读取需要会话 cookie；写入还需要 `X-CSRF-Token`。一次性引导令牌只通过地址 fragment 交给页面，页面立即清除 fragment，再通过 `/auth/exchange` 交换。错误码为稳定机器标识，错误说明为用户可读文本。密钥字段标记 `writeOnly`，任何读取接口都不得返回密钥。
