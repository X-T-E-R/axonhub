# Task49 多节点产品合同与交付

本候选基于 `d9a9f7e2`，加入共享权威 PG18、Axon 专属 Redis、channel/account 唯一执行 owner 和持久 pending 计量。默认 `cluster.enabled=false` 保留单节点入口与计量方式。Worker、原域切流、生产设置与部署由父任务继续实施；本候选没有 raw 路由变更。

## 配置两节点

两节点使用同一份 `nodes`、`channels`、PG 主库和 Redis namespace，仅 `node_id` 不同。以下 ID 是示例，生产 placement 应从当前渠道与公开 alias/Key profiles 清单生成，不复制示例 ID。

```yaml
cluster:
  enabled: true
  node_id: gcp                 # 控制节点填 lag
  control_node: lag
  nodes:
    lag: https://CONTROL_ORIGIN
    gcp: https://DATA_ORIGIN
  channels:
    42: {owner: gcp, identity: upstream-account-42}
    43: {owner: lag, identity: upstream-account-43}
db:
  dialect: postgres
  disable_auto_migration: true
  max_open_conns: 4
  max_idle_conns: 1
  conn_max_idle_time: 1m
cache:
  mode: redis
  namespace: axonhub
```

通过现有秘密配置机制提供 `AXONHUB_DB_DSN`、`AXONHUB_CACHE_REDIS_URL`，不把凭据放进提交。`AXONHUB_CLUSTER_NODES`、`AXONHUB_CLUSTER_CHANNELS` 接受 JSON map，配置测试已覆盖。

部署连接复用 Task48 已验收的 `wg-task48`：lag `10.77.48.1/32` ↔ GCP `10.77.48.2/32`，默认路由和出口保持原状。父窗口需为真实 Axon PG/专属 Redis 配置仅限该链路的监听与访问权限，再验证实际服务连接；已有双向内网 SSH 证据不代替 PG/Redis 连通性。接口来源是主树 `outputs/gcp-vpn-subscription-20261104/{nodes.json,README.md,network-verification.json}`。保留 GCP e2-small/20GiB 和 Xray 192MiB 限额，Sub2 的 PG/Redis 仍独立。

- 所有 enabled channel 必须有 owner；同一 upstream account 的渠道使用同一稳定 `identity`，包括 OAuth token 轮换后的渠道。配置拒绝一个 identity 或当前相同凭据分属不同 owner。
- 一个 `node_id` 同时只运行一个进程。变更 placement 时先停 admission、drain 并停止旧 owner，再启动新 owner；没有超时抢占或凭据双活。
- cluster 要求 PostgreSQL、无 read replica、纯 Redis、API 认证；数据节点必须关闭自动迁移。迁移在维护窗口只由控制节点执行一次，之后控制节点也可关闭自动迁移。
- origins 是管理员维护的 allowlist。普通推理请求头不能选 channel/owner。管理测试与 playground 保留原来的显式 channel 选择和权限检查。
- 4/1 PG 池是本地双实例 fixture 使用的有界起点，不是 GCP 实测容量结论。生产继续验证 e2-small 与既有 Sub2/VPN 的共存内存、磁盘和并发，不默认扩规格。

全局目录保留所有 channel/model/alias 元数据，非 owner 不构造 provider、HTTP client 或 OAuth refresher。推理候选只允许本节点 owner，并在执行前从主库重查 channel enabled/tags 和当前 Key/组上界。`backup`、`gc`、`managed-observability-capacity-gc`、`channel-probe` 仅 control 执行；model-sync、key-health、provider-quota 与 video-storage 按 channel owner 分工。本地 writer、cache、观测消费者继续运行。cluster 启动不再对全库旧 processing 行做单节点 stale cleanup。

## Worker 消费的接口

这些路由均在基础 Ent/logging/tracing middleware 后注册。管理接口沿原 JWT、legacy-user、project 链；ClientKey 接口沿原 IP blocklist、fresh APIKey Auth。接口设置 `Cache-Control: no-store`。

### 模型

- `GET /admin/cluster/manifest`：control 上的管理员 JWT，需 `read_channels`。返回 `version:1`、`generated_at`、管理员 `nodes` map、`models:[{model,owner?,owners,status}]`，不包含 Key 或凭据。
- `GET /v1/axon/route?model=<公开模型>`：原 ClientKey，返回同一模型 route。`200 ready` 是唯一 owner；`409 ambiguous` 表示同 Key 的候选跨 owner；`404 unavailable` 表示无可用授权候选；基础 Auth 的 401/403 原样拒绝；查询失败为 503。

全局 alias 可能因不同 Key 映射而 ambiguous，但每个 Key 的 route 仍可 ready。若同 Key 的候选跨 owner，接口保持 409，不随机选节点。切流前对实际待迁移 Key/公开 alias 集合确认确定 owner；父维护的 Gemini placement 优先于 Responses 等协议默认值。产品不硬编码 Gemini、生产 ID 或 Worker 默认节点。

### 持久异步任务

`GET /v1/axon/task-route?protocol=openai|doubao&method=GET|DELETE&task_id=<opaque ID>` 使用原 ClientKey，按已授权 `Request.channel_id` 返回最小 `{"status":"ready","owner":"gcp"}`。解析不请求上游、不根据 opaque ID 猜 owner、不接收客户端 channel、不保存 Key/任务 KV 关系。400 是输入无效；401/403 来自原 Auth；404 统一表示不存在或无权限；503 表示没有配置 owner。处理前拒绝带 `execution:not_executed`。

原调用入口仍可直接使用：

- `GET|DELETE /v1/videos/:id`
- `GET|DELETE /doubao/v3/contents/generations/tasks/:id`

请求落到非 owner 时，产品只向配置 owner 代理一次，owner 再读当前 Key/用户/组、任务个人归属、model 与 channel/tags。两个查询协议只控制返回表示，保留跨协议查询已有任务的能力。PortalManaged Key 只能操作自己 Key 与 project 的任务；legacy/shared Key 不新增 creator 存在或 membership 前提。授权拒绝不暴露任务是否存在。

DELETE 已发送而结果未知时不换节点重发。已有完成状态在删除后保留；processing 任务删除可标记 canceled，沿既有 terminal-state fencing。后台 video-storage 在各 owner 只查询自己渠道的未保存任务，直接使用本地 provider，避免 control 缺 Outbound 与双节点重复轮询。

### 管理执行

原管理 GraphQL 的 channel test、key-health、model fetch/sync、`resetChannelQuotaNow` 及显式渠道 playground 可在 control 使用；实际 provider 操作分发给 owner，并重新认证原调用者。内部 `POST /__axon/admin/channel-{test,health,models,sync,quota-reset}` 沿 JWT 链，对应 `/__axon/openapi/...` 沿原 OpenAPI principal 链；`POST /__axon/admin/playground/chat` 保留 JWT/project/source。quota-reset 输入仅 `{"channel_id":<ID>}`，需原 `write_channels`，成功返回 `200 true`；scope 不足为 403、主体失效为 401、非 owner 为 409，均不请求上游；已发送但结果未知的 reset 不重放。具原 `read_channels` 的显式管理 test 仍可测试 disabled 渠道，不能越过 owner/当前标签限制；普通推理继续要求 fresh enabled。管理页面、门户、注册仍只在 control。

## 不把正文经共享 PG 回传 lag

采用两节点共享捕获全关，lag 和 GCP 都不再捕获新 request/execution body、response、chunks 或 live preview；usage、共享 pending、账号、Key、组与渠道配置继续保留，不新增对象存储。

`StoragePolicy` 存在共享 PG 的 `systems` 表唯一 `key="storage_policy"` 行，没有 node/project 维度；两节点查询同一行。渠道 `settings` 的三项 execution 覆盖也存在同一共享 channel 行，按 channel 而非节点生效，不能单独控制父 request body/chunks 或 live preview。现有 `conf`/环境变量没有 node-local 捕获开关，因此不能靠 GCP 的单独 YAML 或 channel 覆盖实现“仅 GCP 全关、lag 仍捕获”。

在 control 的 `POST /admin/graphql` 使用原管理员 JWT 与 settings 写权限（owner 可写），执行现有 patch mutation；未提供的 cleanup/capacity 字段保留原值：

```graphql
mutation {
  updateStoragePolicy(input: {
    storeRequestBody: false
    storeExecutionRequestBody: false
    storeResponseBody: false
    storeChunks: false
    livePreview: false
  })
}
```

读回：

```graphql
query {
  storagePolicy {
    storeRequestBody storeExecutionRequestBody storeResponseBody
    storeChunks livePreview
  }
}
```

对应持久 JSON 字段是 `store_request_body`、`store_execution_request_body`、`store_response_body`、`store_chunks`、`live_preview`。所有渠道 settings 的 `storeExecutionRequestBody`、`storeExecutionResponseBody`、`storeExecutionStreamChunks` 必须为 null 或 false；覆盖值 true 会重新开启 execution 捕获。渠道入口是同一 control GraphQL 的 `updateChannel(id:, input:{settings:...})`，沿原 `write_channels` 权限。先读并保留完整现有 settings，仅将三项置 false，再提交完整 settings；该对象并非通用三字段 patch，直接只传三项可能清掉其它配置。不要改 credentials 或其它渠道字段。

设置窗口先停两节点 admission 并 drain 旧 writer，再在 control 更新五项、清理渠道覆盖并重载渠道缓存。mutation 更新 control 的 writer policy；GCP 在设置完成后启动，启动时从 primary 读取 `StoragePolicyFresh`。若数据节点已运行，须在停 admission、drain 并停止旧进程后以新进程启动，不能把 control 的本地 atomic policy 更新当成跨节点即时通知。writer 后续每个持久任务也读取 fresh policy。读回五项和渠道覆盖后才恢复 admission，保证新流量从开始就不捕获 live preview。关闭捕获不自动删除既有历史；本实现不删除账号、Key、组配置或 usage/pending。

真实两服务 fixture 从五项开启开始，只向 control 发送一次原 admin GraphQL `updateStoragePolicy`，随后启动数据节点；两独立 PG 池均读回五项 false，原 cleanup 设置保留，control HTTP query 也返回 false。此后原 HTTP、SSE、WS、管理和视频闭环继续通过：request/execution body、headers 仅空或 `{}`，response body/chunks 为空、payload reference 为 nil、`observability_payloads` 为 0，usage_logs 保留实际 token、pending 已交接。源码依据：`system.go:1035–1187`、`request.go:85–137`、`managed_storage_policy_fresh.go` 与 `cluster_integration_test.go`。完整 body 开启时会写同一 PG 的 `observability_payloads.data` 或 response/chunks 列，仅换 database storage 名称不能避免回流。

## 计量迁移与恢复

完整步骤见 [task49-accounting-design.md](task49-accounting-design.md)。Ent 新增 `pending_usages`，没有父记录 cascade，不含请求/回复/headers/凭据。captured usage 先同步发布紧凑事实，再入本地异步队列；writer 在同一事务内锁 pending、写 usage_logs、删除 pending。quota 在 repeatable-read 快照里读取两表，窗口使用 capture time，未知 cost 保持 NULL，不做预测预扣。无 quota 的实际用量也保留。

Chat 流持续覆盖紧凑 usage 投影，到 provider 的 `[DONE]` 或 clean EOF 才冻结最终用量。cluster 的 semantic-completion 确认在 required capture 完成前保持 pending；若 transformer 先合成正常 terminal，也由同一 reader 读完 provider 尾部，再交付成功或 accounting 错误，不等可选 Close I/O。读取/取消沿原 context，不扩大固定 grace 等待；standalone 原 semantic fallback 保留。同步发布失败也把同一冻结 usage ID/时间/tokens/nullable cost 交给已有 required writer 重试发布与交接；HTTP 返回已执行的 accounting error，SSE/WS 在正常完成前显露失败，网关与调用者均不重放上游。

进程重启只恢复自己 node 的 pending；丢失异步 parent 时建紧凑 terminal skeleton，再原子交接。pending 不按时间过期。

新 admission 开始后，回退必须保留最新 PG 及其中全部 usage/pending、账号和配置事实。先停止 admission、drain 全节点，再用新 writer 完成每个 node 的 pending 交接；确认表空后，停数据节点并让旧 reader 使用同一最新 PG。保留 additive 表，旧 binary 忽略它。drain 或交接受阻时保持新 reader/维护态与 admission 关闭，不启旧 quota reader，不还原维护前快照丢弃已执行用量。

## 验证与接续

定向验证使用 PostgreSQL 18.0、Redis 8.8 的本任务隔离容器、两个独立完整 HTTP/WS service graph 和合成 provider，无真实 provider Key。

- `go test ./internal/server ./internal/server/biz -run '^TestCluster' -count=1 -v`：双实例 HTTP/WS fresh Auth、组/标签撤权、legacy Key、scope、alias/Key 消歧与 409、owner RPC/playground、两协议视频、后台 owner、无正文、PG snapshot/handoff/recovery、additive migration/drained rollback，全部通过（`bash-6seiac9w`）。
- `go test ./internal/server -run '^TestCluster' -count=1 -v`：加入管理 model fetch/sync 后复检同一双服务闭环，证明 provider 调用只在 owner；上游接收 DELETE 后断开连接，断言只发送一次，通过（`bash-zx5et1zv`）。
- `go test ./internal/server -run '^TestClusterTwoHTTPServicesAuthorizationAndOwnership$' -count=1 -v`：本次增量以单次真实 admin GraphQL 更新取代各实例重复设置，证明共享五 false、保留 cleanup、原 query 读回及后续无正文/实际 usage 闭环，通过（`bash-9wlpxl07`，3.91s）。
- `go test ./conf ./internal/pkg/xcache ./internal/server/api ./internal/server/biz ./internal/server/orchestrator ./internal/server/scheduler ./internal/server/video_storage -run 'TestCluster|TestForwardingObservation|TestPortalMappedCatalog|TestOIDCService_PKCE_Flow|TestDataStorageService_GetDataStorageByID_WithRedis|TestGroupAccess|TestQuota' -count=1`：相关旧合同与真实 Redis 两 client 的域隔离/定向 Clear/Invalidate，通过（`bash-0ezm3u3o`）。API 在此 regex 下只有编译，video_storage 的真实回调由双服务测试覆盖。
- F1/F2/F4 准入修复：`go test ./internal/server ./internal/server/api -run '^TestClusterTwoHTTPServicesAuthorizationAndOwnership$|TestWriteSSEStream_(GenuineErrorDuringSemanticGraceWins|ErrorFormatsAsJSON)$|TestResponsesWebSocketActualPipelineProviderErrorBoundary$' -count=1 -v -timeout 90s` 通过（`bash-zp45xgd6`，双服务 6.71s）。启用 same/cross-channel retry 与本 owner 备用候选后，故障 HTTP 500、Responses SSE 失败、Chat SSE `event:error`、WS `accounting_failed` 均不含 `not_executed`，没有正常完成 terminal；每代 provider=1，恢复 usage=1/pending=0，冻结 ID/时间/tokens/nullable cost 不变。真实管理 RPC 证明 disabled test 可用而普通推理被拒，quota reset control=0/owner=1、拒绝=0、unknown 不重放。
- F1 最终 usage/慢发布复检：永久回归 `TestObservationAccounting*` 在 `observation_lifetime_test.go`。红测 `bash-5bo4kma1` 重现 cluster 连续 usage 3→4→5 记成 3，以及 2.5s 发布失败被 semantic grace 提前发 DONE；`bash-v0rbxhmn` 重现 Chat→Messages 合成正常 terminal 后才在 Close 出 accounting 错误。修复后 `bash-omrl38bi` 的三个包全部通过：最终 tokens=5；transformed/pass-through 慢失败无正常 DONE、客户端显露 accounting 错误，provider=1、恢复 usage=1/pending=0，同一 ID/时间/tokens/nullable cost 不变；Messages 合成 terminal 同样受门禁。EOF/取消均小于 1s，standalone semantic fallback、中断/reader join、no-body 紧凑投影与 PG18 双服务原四协议快失败路径一并通过。只缓存成功确认的回归在 `chat_test.go`。
- F3：`TestClusterMonitoringOwnerSchedule` 与 `TestForwardingObservation*` 通过（`bash-dq31dnpm`）；原监控反例连续三轮 a 比 b 早 1 秒，双方各执行本 owner 渠道一次。受影响流/WS/pass-through/GroupAccess 回归通过（`bash-v106etue`），保 standalone HTTP 不等待 transformed Close；Chat usage-only 紧凑投影与受影响流回归通过（`bash-shz9uq2l` 的 orchestrator 包），不保存 content/未知大字段。
- Redis store 定向单测覆盖 namespace/domain、同标签跨域、安全状态 legacy raw key 兼容、300 keys 的 SCAN 清理；不再调用 FLUSHALL。OIDC、OAuth state/device 与 CyberSessionBlock 显式保留原安全状态 key，不从通用 cache 域隐式双读。

测试环境变量：`AXONHUB_TEST_PG_DSN`、`AXONHUB_TEST_PG_DISPOSABLE=1`、`AXONHUB_TEST_REDIS_URL`。每个 PG 测试只建/删自己的随机 schema，Redis 测试只删自己的 namespace/合成键。上述任务 ID 对应本次 session 的完整执行日志。

父接续：审读候选、依 fresh 生产清单生成同一 placement，确认 Gemini 公开 alias/Key 路由到 GCP；完成 Worker→GCP Axon→GCP Sub2/对应上游的原域直路、五项 StoragePolicy/渠道覆盖读回、私网连接与共存资源实测，再在明确窗口切流。产品实现阶段未 SSH、未生产写、未构建或部署。
