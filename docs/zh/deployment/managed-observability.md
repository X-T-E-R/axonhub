# 托管可观测数据存储

AxonHub 现将请求日志骨架行与大型请求体字节分离。这是增量式双读变更：旧的内联 JSON 和外部存储记录仍可读取，启动时不会破坏性改写旧数据。

## 精确存储结果

| 设置 | 新记录结果 |
| --- | --- |
| `store_request_body=false` | `requests.request_body` 为 `{}`，不接纳托管父请求载荷。 |
| `store_request_body=true` | 使用主数据库存储时，规范入站字节存入 `observability_payloads`；`requests.request_body_payload_id` 引用该载荷，必需的旧列 `requests.request_body` 保持 `{}`。非主外部存储继续使用现有对象键和处置路径。 |
| 缺少 `store_execution_request_body` | 继承 `store_request_body`，保持旧版共享开关行为。 |
| `store_execution_request_body=false` | 保留执行骨架，但不创建请求体引用；渠道覆盖可重新启用。 |
| `store_execution_request_body=true` | 最终供应商请求字节使用同一请求组目录。字节完全相同的重试/故障转移引用一个物理载荷；转换、透传、模型/URL/请求头覆盖产生的版本按字节比较，每种不同字节序列仅存一次。渠道覆盖优先。 |
| 缺少 `managed_observability_hard_mib` / `managed_observability_low_mib` | 为兼容旧部署而禁用容量模式；托管精确去重仍生效。 |

哈希和长度只用于筛选候选，AxonHub 会在复用前比较最终持久化字节。容量淘汰后，骨架仍保存 SHA-256、字节长度和处置原因。响应体与响应块开关继续保持现有的父级/执行级共享语义。

启用容量模式后，即使父级和执行级请求体捕获都关闭，主数据库中的 Request/RequestExecution 请求组仍标记为托管。容量核算由保守的载荷计费、实际驻留数据库的响应/请求头/响应块 JSON 大小和骨架固定余量组成，明确白名单仅包括托管 Requests、RequestExecutions、其载荷、关联 Trace/Thread 诊断行和 Usage Logs；非主外部对象字节不属于此数据库容量预算。清理先淘汰载荷字节并保留摘要/长度/处置原因；只有仍无法达到低水位时，才通过现有完整性检查删除完整的终态托管请求组及其 Usage 行。绝不会选择账户、API 密钥、项目、路由、供应商、渠道或系统配置。先处理成功请求组，再处理包含失败证据的请求组；只要父级或任一子执行仍为 pending/processing，该组即为活动组，即使父级元数据矛盾地显示为终态，也不会被选择。PostgreSQL 工作实例竞争同一个非等待会话 advisory lock，并通过专用连接在保留期清理、容量清理和普通 VACUUM 全程持有。SQLite/MySQL 的所有者保证明确仅限单进程。

容量压力采用 fail-open：供应商转发与请求/执行骨架状态转换继续可用，但新的主数据库请求体、请求头、响应体、响应块和 Usage Logs 可能跳过接纳。结构化 `managed_observability_*` 信号、`managed_observability_failures_total{component,reason}` 指标以及单例 `managed_observability_states.last_error` 的组件值共同描述接纳、所有者锁、解锁和清理降级。公开健康状态会报告该组件，但仍保持健康；不得仅因容量压力就让健康/存活检查失败。

运行时自动迁移明确使用 `WithForeignKeys(false)`，所以 Ent 声明的 cascade/set-null 边不是生命周期权威。保留期和容量删除会在同一个应用事务中显式删除请求拥有的载荷目录行并核对计费字节；旧版内联请求组沿用同一删除路径，不要求存在载荷行。

## 分阶段部署

指定的数据库迁移负责人应按以下顺序执行：

1. 保持临时策略（`store_request_body=false`、`store_response_body=true`、`store_chunks=false`、`live_preview=false`、Requests 1 天、Usage Logs 30 天）。
2. 部署双读 schema/代码，通过 GraphQL 和诊断验证旧内联及外部记录；容量字段暂不设置。
3. 设置 `managed_observability_hard_mib=10240` 和 `managed_observability_low_mib=8192`；验证只有一个 PostgreSQL GC 所有者并观察接纳/清理信号。
4. 启用 `store_request_body=true` 和 `store_execution_request_body=true`。自动精确去重会避免相同执行副本；渠道级执行覆盖仍可单独开关。

可运行 `scripts/postgres-managed-observability-integration.sh` 进行一次性可丢弃发布证明。脚本不接受 DSN，会创建全新 PostgreSQL，使用真实运行时迁移和应用接纳/GC/锁路径执行 8 次硬/低水位循环，终止实际 advisory-lock 会话以验证释放，然后运行有界的 24 MiB heap/TOAST/索引/VACUUM 复用烟测；它绝不会连接操作员提供的数据库。

Requests 24 小时和 Usage 30 天都是最长保留值。按已观测到的父请求体速率（约 25 小时 15 GiB），10/8 GiB 预算会在 24 小时之前淘汰成功请求载荷。

## 现有约 30 GB 数据与物理空间

不要在启动时破坏性改写，也不要例行使用 `VACUUM FULL`。普通删除加 `VACUUM (ANALYZE)` 会让 heap/TOAST 页面可复用，并应使托管载荷关系的分配量进入平台期，但不保证把历史关系文件归还给文件系统。

若需要一次性收缩文件系统，应使用另行审批的在线改写工具（例如 `pg_repack`），并先处理执行表：

1. 预检扩展/工具版本兼容性、阻塞 DDL、长事务、复制延迟和备份恢复。分别测量 `pg_total_relation_size`、heap、TOAST、索引、死元组、可复用页面及文件系统剩余空间。
2. 为目标关系及其索引预留峰值空间，并额外保留至少 20% 运行余量；不足时在开始前中止。
3. 先改写旧 `request_executions`，验证读取和转发，再改写 `requests`。只允许一个改写负责人，并监控锁和延迟。
4. 在行数、旧数据读取、诊断和存储测量全部通过前保留改写前备份/快照。回滚边界是表交换：交换前可取消并保留旧关系；交换后应从已验证备份恢复，不进行就地反向改写。

只有在尚未接纳新的纯托管记录前，回滚到旧二进制才安全。启用父级/执行级捕获后，旧二进制只能看到必需的 `{}` 占位；应用降级前必须把托管载荷回填到旧表示，或继续使用双读版本。
