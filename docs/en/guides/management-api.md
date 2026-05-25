# Management API

The management API lets trusted automation manage AxonHub service-account tokens, channels, and channel provider keys without using the browser UI or GraphQL directly.

Use two authentication modes:

- Admin token-management routes under `/admin/management/tokens` use an administrator JWT.
- Management routes under `/openapi/v1/management` use a service-account management token.

Responses never return raw upstream provider keys. Token creation returns the new management token value once; list and get responses return only masked token values.

## Environment

```bash
export AXONHUB_URL="https://axonhub.example.com"
export ADMIN_JWT="admin-jwt"
export MANAGEMENT_TOKEN="management-token"
export CHANNEL_ID="1"
export KEY_ID="provider-key-fingerprint"
export PROVIDER_KEY="upstream-provider-key"
```

## Create A Management Token

Requires an administrator JWT and `write_api_keys`.

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

Store the `key` value from this response securely. It is not returned again.

## Discover Capabilities

Requires an enabled service-account management token. The response includes both `/openapi/v1/management/*` operations and administrator JWT token-management operations.

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/capabilities" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

## List Channels

Requires `read_channels`.

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/channels" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

## Read Channel Key Inventory

Requires `read_channels`. Inventory returns fingerprint IDs, masked keys, key status, health metadata, and balance metadata when available.

```bash
curl -sS "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

Use an inventory item `id` as `$KEY_ID` for targeted key operations.

## Add Provider Keys

Requires `write_channels`. Submitted provider keys are accepted in the request body but are not echoed in the response.

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "keys": ["'"$PROVIDER_KEY"'"],
    "runHealthCheck": false
  }'
```

## Run Health Checks

Prefer targeted health checks for automation.

Run a health check for exactly one key:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/health-check" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

If `$KEY_ID` does not match an inventory item ID or raw provider key, the endpoint returns `404`.

Run health checks for a small selected set:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/health-check" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "keyIds": ["'"$KEY_ID"'"]
  }'
```

If `keyIds` is omitted on the selected-key endpoint, AxonHub preserves the existing all-routable-key behavior. If `keyIds` is provided but empty after trimming, the request is rejected with `400`. Use the one-key path or explicit `keyIds` for predictable automation.

## Disable, Enable, Archive, Restore, And Delete Keys

All key mutation routes require `write_channels`.

Disable a key:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/disable" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"maintenance"}'
```

Enable a key:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/enable" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

Archive a key:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/archive" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"rotated"}'
```

Restore an archived key:

```bash
curl -sS -X POST "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID/restore" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```

Delete a key:

```bash
curl -sS -X DELETE "$AXONHUB_URL/openapi/v1/management/channels/$CHANNEL_ID/keys/$KEY_ID" \
  -H "Authorization: Bearer $MANAGEMENT_TOKEN"
```
