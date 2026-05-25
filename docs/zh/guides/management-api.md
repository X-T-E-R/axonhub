# 管理 API

管理 API 用于让可信自动化脚本管理 AxonHub 的服务账号令牌、渠道和渠道上游密钥，不需要进入浏览器界面，也不需要直接调用 GraphQL。

API 使用两类认证：

- `/admin/management/tokens` 下的管理令牌接口使用管理员 JWT。
- `/openapi/v1/management` 下的管理接口使用服务账号管理令牌。

响应不会返回上游 provider key 明文。创建管理令牌时只会返回一次新令牌的 `key` 值；列表和详情接口只返回脱敏后的令牌。

## 环境变量

```bash
export AXONHUB_URL="https://axonhub.example.com"
export ADMIN_JWT="admin-jwt"
export MANAGEMENT_TOKEN="management-token"
export CHANNEL_ID="1"
export KEY_ID="provider-key-fingerprint"
export PROVIDER_KEY="upstream-provider-key"
```

## 创建管理令牌

需要管理员 JWT 和 `write_api_keys` 权限。

```bash
curl -sS -X POST "$AXONHUB_URL/admin/management/tokens" \
  -H "Authorization: Bearer $ADMIN_JWT" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ops-bot",
    "projectId": 1,
    "scopes": ["read_channels", "write_channels"]
  }'
```

请安全保存响应里的 `key` 值。这个明文令牌之后不会再次返回。

## 查询能力清单

需要启用状态的服务账号管理令牌。响应会同时列出 `/openapi/v1/management/*` 操作和管理员 JWT 的令牌管理操作。

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/capabilities" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

## 列出渠道

需要 `read_channels` 权限。

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/channels" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

## 查询渠道密钥清单

需要 `read_channels` 权限。清单会返回指纹 ID、脱敏密钥、状态、健康检查元数据和可用的余额信息。

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

清单项里的 `id` 可作为 `$KEY_ID` 用于后续定向操作。

## 添加上游密钥

需要 `write_channels` 权限。请求体可以提交上游 provider key 明文，但响应不会回显这些明文。

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "keys": ["'"$PROVIDER_KEY"'"],
    "runHealthCheck": false
  }'
```

## 执行健康检查

自动化脚本优先使用定向健康检查。

对单个密钥执行健康检查：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/health-check" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

如果 `$KEY_ID` 无法匹配清单项 ID 或上游 provider key，接口会返回 `404`。

对少量指定密钥执行健康检查：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/health-check" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "keyIds": ["'"$KEY_ID"'"]
  }'
```

如果在选定密钥接口中省略 `keyIds`，AxonHub 会保留现有的全部可路由密钥检查行为。如果提供了 `keyIds` 但裁剪后为空，接口会返回 `400`。自动化场景建议使用单密钥路径或显式 `keyIds`，避免误触发批量检查。

## 禁用、启用、归档、恢复和删除密钥

所有密钥变更接口都需要 `write_channels` 权限。

禁用密钥：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/disable" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"maintenance"}'
```

启用密钥：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/enable" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

归档密钥：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/archive" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"rotated"}'
```

恢复已归档密钥：

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/restore" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

删除密钥：

```bash
curl -sS -X DELETE "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```
