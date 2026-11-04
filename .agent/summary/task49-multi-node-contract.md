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

原管理 GraphQL 的 channel test、key-health、model fetch/sync 及显式渠道 playground 可在 control 使用；实际 provider 操作分发给 owner，并重新认证原调用者。内部 `POST /__axon/admin/channel-{test,health,models,sync}` 沿 JWT 链，对应 `/__axon/openapi/...` 沿原 OpenAPI principal 链；`POST /__axon/admin/playground/chat` 保留 JWT/project/source。scope 不足在执行前拒绝，管理页面、门户、注册仍只在 control。

## 不把正文经共享 PG 回传 lag

使用现有共享 StoragePolicy，关闭五项，不新增对象存储：

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

对应持久 JSON 字段是 `store_request_body`、`store_execution_request_body`、`store_response_body`、`store_chunks`、`live_preview`。所有渠道 settings 的 `storeExecutionRequestBody`、`storeExecutionResponseBody`、`storeExecutionStreamChunks` 必须为 null 或 false；覆盖值 true 会重新开启 execution 捕获。保留其余渠道字段、账号、Key、组配置及 usage/pending。

设置窗口先停 admission 并 drain 旧 writer，再更新设置、清理这些渠道覆盖并重载两节点渠道缓存。writer 每个持久任务都从 primary 读取 `StoragePolicyFresh`；读回五项和各渠道覆盖后才恢复新 admission。已经存下的历史不会因关闭捕获自动删除；用户允许历史可丢不等于本实现自动删除账号或配置。

真实两服务 fixture 已完成 HTTP 和 SSE：两 PG 池读到 request/execution 的 request body、headers 仅空或 `{}`，response body/chunks 为空、payload reference 为 nil、`observability_payloads` 为 0；usage_logs 保留实际 token、pending 已交接。完整 body 开启时会写同一 PG 的 `observability_payloads.data` 或 response/chunks 列，因此仅换 database storage 名称不能避免回流。见 `request.go` 的捕获 gates、`managed_storage_policy_fresh.go` 和 `cluster_integration_test.go`。

## 计量迁移与恢复

完整步骤见 [task49-accounting-design.md](task49-accounting-design.md)。Ent 新增 `pending_usages`，没有父记录 cascade，不含请求/回复/headers/凭据。captured usage 先同步发布紧凑事实，再入本地异步队列；writer 在同一事务内锁 pending、写 usage_logs、删除 pending。quota 在 repeatable-read 快照里读取两表，窗口使用 capture time，未知 cost 保持 NULL，不做预测预扣。无 quota 的实际用量也保留。

进程重启只恢复自己 node 的 pending；丢失异步 parent 时建紧凑 terminal skeleton，再原子交接。pending 不按时间过期。PG 不可用时发布失败会返回 accounting error，上游可能已执行，调用者不重放。

回退旧单节点 reader 前，停止新请求并 drain 两节点；用新 writer 恢复所有 pending，确认表空，再停数据节点并切回旧入口。保留 additive 表，旧 binary 忽略它。pending 未清空时不能启旧 quota reader；继续新 reader 或恢复维护前 PG 快照及其匹配的应用状态。

## 验证与接续

定向验证使用 PostgreSQL 18.0、Redis 8.8 的本任务隔离容器、两个独立完整 HTTP/WS service graph 和合成 provider，无真实 provider Key。

- `go test ./internal/server ./internal/server/biz -run '^TestCluster' -count=1 -v`：双实例 HTTP/WS fresh Auth、组/标签撤权、legacy Key、scope、alias/Key 消歧与 409、owner RPC/playground、两协议视频、后台 owner、无正文、PG snapshot/handoff/recovery、additive migration/drained rollback，全部通过（`bash-6seiac9w`）。
- `go test ./internal/server -run '^TestCluster' -count=1 -v`：加入管理 model fetch/sync 后复检同一双服务闭环，证明 provider 调用只在 owner；上游接收 DELETE 后断开连接，断言只发送一次，通过（`bash-zx5et1zv`）。
- `go test ./conf ./internal/pkg/xcache ./internal/server/api ./internal/server/biz ./internal/server/orchestrator ./internal/server/scheduler ./internal/server/video_storage -run 'TestCluster|TestForwardingObservation|TestPortalMappedCatalog|TestOIDCService_PKCE_Flow|TestDataStorageService_GetDataStorageByID_WithRedis|TestGroupAccess|TestQuota' -count=1`：相关旧合同与真实 Redis 两 client 的域隔离/定向 Clear/Invalidate，通过（`bash-0ezm3u3o`）。API 在此 regex 下只有编译，video_storage 的真实回调由双服务测试覆盖。
- Redis store 定向单测覆盖 namespace/domain、同标签跨域、安全状态 legacy raw key 兼容、300 keys 的 SCAN 清理；不再调用 FLUSHALL。OIDC、OAuth state/device 与 CyberSessionBlock 显式保留原安全状态 key，不从通用 cache 域隐式双读。

测试环境变量：`AXONHUB_TEST_PG_DSN`、`AXONHUB_TEST_PG_DISPOSABLE=1`、`AXONHUB_TEST_REDIS_URL`。每个 PG 测试只建/删自己的随机 schema，Redis 测试只删自己的 namespace/合成键。上述任务 ID 对应本次 session 的完整执行日志。

父接续：审读候选、依 fresh 生产清单生成同一 placement，确认 Gemini 公开 alias/Key 路由到 GCP；完成 Worker→GCP Axon→GCP Sub2/对应上游的原域直路、五项 StoragePolicy/渠道覆盖读回、私网连接与共存资源实测，再在明确窗口切流。产品实现阶段未 SSH、未生产写、未构建或部署。
