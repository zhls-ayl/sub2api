# Excel / BPS 协议（最小移植版）

BPS = Basis Points，是 OpenAI 官方 Excel 插件（`bps.openai.com`）的后端 API。本仓库把它作为独立的账号类型 **BPS OAuth（`bps-oauth`）**：通过官方 Excel 客户端授权的 OpenAI 账号，按账号配置的模型把 `/v1/responses` 请求经协议适配层改写为 Excel 插件协议发出。协议适配层移植自 ranxi2001/sub2api（其 basispoints 包源自 hloolx/codex2api，MIT）。

## 与 ranxi 原版的差异（本部署裁剪）

- **BPS-only 账号类型**：BPS 只接受官方 Excel 客户端（`client_id = app_fnr0pYvVwwFDocDumLG3H2Bp`）签发的 token。ranxi 会为普通 Codex 账号另外申请一份独立的 Excel 授权（双凭据 + 自动重登 worker），本构建没有移植这部分，因此：
  - BPS 只属于 `bps-oauth` 账号，没有账号级开关，也没有 ranxi 的全局 BPS 开关（关掉它在这里只等于停用全部 bps-oauth 账号）；普通 Codex OAuth（`oauth`）账号不能走 BPS。
  - 账号类型与凭据必须一致：`bps-oauth` 必须持有 Excel client 的凭据，`oauth` / `setup-token` 账号不能持有 Excel client 的凭据，否则保存返回 400 `BPS_OAUTH_INVALID`。`bps-oauth` 账号不能改成其他类型，其他类型也不能改成 `bps-oauth`，需要时新建账号。
- **无 mihomo 会话代理 / Warm IP 池**：BPS 请求始终走账号自身绑定的代理（`proxy_id`）。需要不同出口 IP 时给账号绑不同代理。
- **无自动重登（reauth/2FA worker）**：RT 失效后由管理员在账号菜单里"重新授权"（走 Excel 客户端）。
- **无 403 自动关闭 / 自动恢复 / 自动移组**：对 BPS-only 账号来说，关闭协议或移组都等于整号不可用，所以普通 BPS 403 直接交给账号级处理（见下文"行为要点"）。
- **图片输入只做原生附件上传**：没有 ranxi 的临时 HTTPS 图片中转（relay）、按图片数自动 compact / 预警策略，也不支持生图（`/v1/images` 不会选中 BPS OAuth 账号，对话内的图片生成托管工具照旧省略）。图片输入始终开启，没有单独开关（见下文"图片输入"）。
- **无 account_quality 联动 / 自动配置模板 / 定时探针 / 请求采集**。

## 建号流程（手动 OAuth）

1. 管理后台 → 添加账号 → OpenAI → **BPS OAuth**，在"模型限制"里填写要服务的模型（默认预填 gpt-6-astra / gpt-5.6-sol / gpt-5.6-terra；留空也按这三个处理）。
2. 生成授权链接（后端用 Excel 专用 client 走 PKCE，state 前缀 `bps.`）。
3. 浏览器打开链接完成 OpenAI 登录授权；无论页面是否提示错误，复制 `bps.openai.com` 地址栏里含 `code` 与 `state` 的完整回调 URL。
4. 粘贴回管理页兑换，系统校验返回的 `client_id` 必须是 Excel client，创建 `bps-oauth` 账号（凭据存 `accounts.credentials`，AT 过期按 credentials 里的 client_id 自动刷新）。也可以直接粘贴 Excel 客户端的 Refresh Token。
5. Excel 会话不能走"导入 Codex 会话"：输入里带 Excel `client_id`、或 access token 的 `client_id` claim 是 Excel client 的条目会直接报错，避免 Excel token 被当成 Codex 账号使用或合并进同一用户的 Codex 账号。

Excel token 的兑换与刷新按官方 Excel 客户端的契约发送：token 端点加 `unified=true`，刷新不带 `scope`，不带 Codex 的 User-Agent / originator 头。失败信息不回显上游 body，只保留状态码和白名单内的 OAuth 错误码（如 `invalid_grant`、`refresh_token_reused`），以便 RT 失效时刷新服务能识别为不可重试并标记账号。

## 能力与限制

- **只服务 BPS**：没有原生 Codex 通道。调度器（高级调度与旧调度规则一致）只把该账号派给它模型限制里的模型（留空时为默认三个模型）；万一已经选中（例如渠道映射或账号的模型限制在选号后变了），转发阶段换号重试（`excel_oauth_route_unavailable`），不直接返回 400。
- **只支持 Responses 接口**：`/v1/chat/completions`、`/v1/messages`、`/v1/messages/count_tokens`、图片接口都不会选中 BPS OAuth 账号。想同时提供 BPS 与非 BPS 模型或接口，把 BPS OAuth 账号和普通 Codex OAuth 账号放在同一个分组里，调度会按模型和入口筛选可用账号。
- **token 计数本地估算**：BPS 没有计数端点，`/v1/responses/input_tokens` 选中 BPS OAuth 账号时直接本地估算，Excel token 不会发往原生 `input_tokens` 端点。
- **只走 HTTP/SSE**：不接受 WebSocket 会话。调度器不会给 WS 会话选 BPS OAuth 账号；万一选中（任何模型或套餐下），转发前直接换号（`basispoints_inbound_unsupported`），不会继续走原生 Codex WS。
- **托管工具**：联网搜索、图片生成等托管工具会被适配层省略，并告知模型能力不可用；客户端函数工具不受影响。
- **图片输入**：见下文"图片输入"。
- **privacy**：建号或重新授权（OAuth 兑换或粘贴 RT）时和 Codex 账号走同一套信息补全：读取套餐与订阅到期时间，并尽力关闭该 ChatGPT 账号的训练数据共享。成功时账号的 `privacy_mode` 记为 `training_off`，要求设置 privacy 的分组会正常选中它；失败时这类分组不会选中它。后台刷新 token 时也会重复这套补全（关闭训练是幂等操作），但结果不回写；后台的 privacy 补设和账号菜单里的"设置 privacy"只作用于 Codex OAuth 账号。
- **Codex 专属功能不适用**：打票、Codex 指纹、WS v2、compact、额度重置（wham）、Spark 影子等都不作用于 BPS OAuth 账号。

## 账号级配置（extra 键）

| 键 | 说明 |
|---|---|
| `openai_excel_bps_ignore_encrypted_content` | 旧会话里 BPS 无法转发的密文替换为省略提示 |
| `openai_excel_bps_cache_creation_as_input` | 缓存创建 token 按普通输入计费 |

BPS OAuth 账号服务哪些模型，只看通用的"模型限制"（白名单或映射，存在 `credentials.model_mapping`）；留空时只服务 `gpt-6-astra`、`gpt-5.6-sol`、`gpt-5.6-terra`。旧版本单独的 BPS 模型白名单 `openai_excel_bps_models` 已删除，留在 extra 里的旧值不再生效（编辑保存时会清掉）。透传开关对 BPS OAuth 账号不生效。

另外沿用通用的长上下文计费（单账号与批量编辑都支持）与 5h/7d 自动暂停设置。和 Codex 订阅账号一样，GPT-6.1 Sol 在 BPS OAuth 账号上不收长上下文附加费。free 套餐账号不能走 BPS；数据导入时也会从 id_token 补全 `plan_type`。

## 配置

- 没有全局开关：BPS 只属于 bps-oauth 账号，要停用 BPS，在账号列表里按类型筛出 BPS OAuth 账号批量停调度即可。
- `config.yaml` `gateway.excel_bps_stream_data_interval_timeout`：流无数据帧超时（秒，0 禁用，30–300，默认 120）。
- `config.yaml` `gateway.excel_bps_state_redis_enabled`：工具回放/目录状态共享 Redis（多实例）；默认 false（进程内）。

## 行为要点

- **调度**：BPS OAuth 账号和原生账号平权。对 BPS 账号模型限制内的模型，两类账号一起按现有规则排序：账号优先级、得分、负载、订阅优先、上游费率等，不会默认偏向 BPS。想让 BPS 优先，就把 bps-oauth 账号的优先级数值设小（数值越小越优先）。开启订阅优先时，订阅池只包含 Codex OAuth 订阅账号，bps-oauth 账号归入普通池。BPS 429 只冷却该账号的 BPS 路由（`excel_bps_rate_limited` 排除原因）并换号重试；模型权限 403（`model_not_allowed` / `basispoints_model_access_changed`）冷却该（账号,模型）30 分钟（`excel_bps_model_not_allowed`）并换号重试，所有账号都不可用时返回 403 `basispoints_model_not_allowed`。这几类换号都不计入账号健康分。
- **粘性会话**：会话留在首轮选中的账号上，不会因为分组里有 BPS 账号（或原生账号）而改派；只有在该账号不可用（冷却、限流、出错换号等）时才换到别的账号。
- **跨协议续接**：混合分组里，会话在换号时可能从原生账号换到 BPS 账号，或者反过来。会话历史里的加密推理和 compaction 密文是生成它的上游私有的，另一侧解不开。两侧的处理方式相同：
  - 上游返回 `invalid_encrypted_content` 时，剥掉加密推理和带密文的 compaction 项，在同一账号上重试一次；
  - 记住这次被拒的密文，之后同一会话的每一轮都先剥掉再发，不用每轮先被拒一次。BPS 和原生各自记录，互不影响。

  代价是丢失隐藏的推理过程，以及压缩前那段历史的细节，对话本身能继续。BPS 侧不会剥离加密的消息正文和函数参数，这类历史仍然返回 400（旧会话可以用账号的"忽略密文"选项）。需要长会话稳定的分组，可以用优先级把 BPS 和原生分层，或者不混用。
- **普通 401/403**：BPS-only 账号没有别的通道，按整号处理并换号重试：
  - 401：清 token 缓存并临时停调度，给 token 刷新留窗口；token 已吊销等情况标记错误。
  - 403：交给 OpenAI 通用 403 处理——HTML / Cloudflare 拦截页不处罚，其余先临时冷却，累计多次才永久停用。
  - 两者都返回固定文案，不透出 BPS 原文；流式修复请求已开始输出时只做整号处理，不换号。
- **402 / 5xx / 529**（尚未输出）：按原生 OpenAI 账号同样的规则处理账号状态——402（如 `deactivated_workspace` 工作区停用）标记账号错误，529 置 overload，其余 5xx 只记日志——然后换号重试。团队联动停用只由原生 OpenAI OAuth 账号触发（目前只在 count_tokens 路径上），触发后会连带同工作区的 BPS OAuth 账号；BPS 自己收到的 402 只停用该账号，不触发联动。所有账号都失败时返回固定文案（402 → 502 "Excel BPS rejected the selected account"，5xx → 502、529 → 503 "Excel BPS upstream is temporarily unavailable"），不透出 BPS 原文。其余 4xx 照旧直接返回。
- **换号耗尽的文案**：BPS 账号拒绝、无 BPS 路由（`excel_oauth_route_unavailable`）、入口不支持（`basispoints_inbound_unsupported`）、上游错误和图片不支持都返回各自固定的 BPS 文案和错误码，不会落到通用的凭据失败文案。
- **token 生命周期**：取 token、后台刷新、手动刷新、重新授权都按 OpenAI OAuth 处理，刷新使用凭据里的 Excel client_id。
- **用量**：5h/7d 窗口来自 BPS 响应头快照与用量日志，不会向 chatgpt.com 发起 Codex 探测。渠道监控可以把 BPS OAuth 账号作为额度数据源。
- **粘性会话冷却**：粘在 BPS 账号上的会话同样受 BPS 429 冷却和模型权限冷却约束，冷却期间改选其他账号。高级调度和旧调度都会在"无可用账号"诊断里标出 BPS 冷却；分组里可用账号全部处于 BPS 429 冷却时返回 429。
- **传输**：BPS 走独立 HTTP 上游 profile（`excel_bps`），经 HTTP 代理出现 H2 流错误时该代理短时熔断降级 HTTP/1.1（1 分钟试探，独立连接池）。
- **计费**：`/basispoints/api/responses` 端点按 Responses 计费；`cache_creation_as_input` 打开后缓存创建并入普通输入。
- **模型列表**：BPS OAuth 账号的测试下拉、普通 `/v1/models`、composite 列表和 Codex manifest 都只按账号配置生成：模型限制里的模型，留空时为默认三个模型。不请求 BPS，也不会列出原生 OpenAI 默认模型；分组里没有 BPS OAuth 账号时，这些列表的结果不变。Codex manifest 里 BPS 模型的推理强度用本地默认值（转发时 max / ultra 按 xhigh 发送），并清掉 multi-agent 字段。已知问题：混合分组的 Codex manifest 会随选中的账号来回变化，暂不处理。
- **同步上游模型**：编辑账号时点"同步上游模型"，实时请求 `GET /basispoints/api/responses/access?include_models=true`（不缓存），返回 BPS 当前对该账号可用的全部模型（已排除受限、未启用和不可用的），加入模型限制。BPS 新出的模型要这样同步或手动添加后才会生效。
- **账号测试**：默认测试走与用户请求相同的网关转发路径，测试模型和用户请求一样用对外名字，只做一次映射；没选模型时用测试下拉里的第一个模型（模型限制留空时是 gpt-6-astra）。另可选"BPS 工具往返"（nonce → echo 工具 → 回传核对，最多 3 次上游请求）。BPS 拒绝请求时，测试弹窗显示上游 HTTP 状态码、Request ID、错误码和脱敏后的错误消息，错误行末尾附 `(HTTP <状态码>)`。

## 图片输入

- **始终开启，原生附件上传**：消息内容（含历史消息）里的 `data:` 内联图片先整批本地校验，全部通过后才上传到 `bps.openai.com/basispoints/api/attachments`（走账号自身代理，60 秒超时），换成 `file_id` 再发 Responses。同一会话（有 `prompt_cache_key` 或会话头）在同一账号、同一 token 下重复的图片复用已上传的 `file_id`（最多 512 条、30 分钟）；没有会话标识的请求不复用。
- **工具输出截图**：`function_call_output` / `custom_tool_call_output` 里校验过的内联截图保持内联发送，不上传。工具输出里的 HTTPS 图片和 `file_id` 图片会挪到紧随其后的一条用户消息里，原位置留文字标签。
- **透传**：HTTPS `image_url` 和合法的 `file_id` 原样透传（`file_id` 只保留 `type` 和 `file_id` 两个字段）。
- **限制**：PNG / JPEG / GIF / WebP；单张 ≤ 20 MiB、≤ 64 MP；单请求内联图片 ≤ 20 张、合计 ≤ 32 MiB；`detail` 只能是 auto / low / high / original。`/v1/responses` 的请求体上限是 `gateway.max_body_size`（默认 256 MiB），32 MiB 图片 base64 后约 43 MiB，不会先被网关拦下。
- **BPS 带不动的图片降级换号**：超限、格式不对、非 HTTPS 地址、引用不合法等本地校验失败不算账号问题——本请求不再选任何 BPS 账号，转给分组里的原生账号处理；没有原生账号时返回 400 `basispoints_request_invalid` 和协议层的提示（只含限额和路径，不含图片内容）。
- **上传失败**（发生在任何生成之前）：
  - 429：只冷却该账号的 BPS 路由，换号。
  - 401 / 402：按整号处理（规则同上），换号。
  - 5xx / 529：按账号策略处理（529 置 overload，其余 5xx 只记日志），换号；连接失败、超时等传输错误同样换号，不改账号状态（重传上传只会多留一个未使用的附件）。
  - 403：不改账号状态，也不换号，直接返回 403 `basispoints_attachment_error`（BPS 可能只是不允许该账号上传附件，文字请求仍可用）。
  - 其余失败（BPS 拒收图片的 4xx、3xx、返回内容不合法）不重放、不改账号状态，返回 `basispoints_attachment_error`，状态码为 BPS 原状态码，3xx 和内容不合法时为 502；上传并发已满返回 503。
  - 对外信息不含 URL、body 或 `file_id`；BPS 错误里回显的 `file-…` 也会脱敏。

## 升级与上游

`backend/internal/service/basispoints/` 为自包含协议包（仅 stdlib + jsonschema + gjson）。bps.openai.com 协议无 SLA；如需跟进协议变化，可对照 ranxi2001/sub2api 或 hloolx/codex2api 的更新做增量移植——包内无本仓库其他依赖，整目录替换的冲突面很小。

## 验证清单（手动 E2E）

1. BPS OAuth 建号，账号类型显示为 "BPS OAuth"；账号测试（默认与 BPS 工具往返）通过。
2. 模型限制内的模型 `/v1/responses`（流式/非流式）经 `basispoints` 正常返回，用量行 `upstream_endpoint=/basispoints/api/responses`。
3. 模型限制外的模型请求不会派到 BPS OAuth 账号；`/v1/chat/completions`、`/v1/messages`、`/v1/messages/count_tokens` 也不会。
4. `/v1/responses/input_tokens` 选中 BPS OAuth 账号 → 返回本地估算值，无上游请求。
5. 编辑 BPS OAuth 账号 → 模型限制区"同步上游模型"返回 BPS 模型列表。
6. （可选）制造 BPS 403（例如换一个被拒的代理）→ 账号临时停调度，请求换到同分组的其他账号。
7. 带 `data:` 图片（含历史消息里的图片）请求模型限制内的模型 → Ops 里先出现 `/basispoints/api/attachments` 上传，Responses 请求体只含 `file_id`；同一会话的下一轮不再重复上传同一张图。
8. 混合分组里发一个超过 20 张内联图片（或 `http://` 图片地址）的请求 → 请求转给原生账号；纯 BPS 分组 → 400 `basispoints_request_invalid`。
