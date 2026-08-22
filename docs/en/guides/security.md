# Security Features

AxonHub provides multiple layers of IP-based access control to secure your AI gateway.

## IP Access Control (Global Allowlist)

IP Access Control is a global middleware that restricts access to the entire AxonHub instance. When enabled, only requests from IP addresses or CIDR ranges in the allowlist are accepted; all others are denied.

This is useful when you want to lock down your AxonHub instance to only accept traffic from known networks (e.g., your corporate VPN or cloud VPC).

### Configuration

IP Access Control is configured via the AxonHub configuration file or environment variables:

```yaml
ip_access_control:
  enabled: true
  allowed_ips:
    - "10.0.0.0/8"
    - "192.168.1.0/24"
    - "203.0.113.10"
  redirect_url: ""  # Optional: redirect denied requests to this URL
```

### Behavior

- When **disabled** (default): all requests are allowed through.
- When **enabled**: requests from IPs matching any entry in `allowed_ips` are allowed; all others receive a 404 (or 302 redirect if `redirect_url` is set).
- The client IP is determined from the TCP connection's remote address.

## IP Blocklist (System-Wide Denylist)

The IP Blocklist allows administrators to block specific IP addresses or CIDR ranges from making API requests. This is managed at runtime through the system settings UI.

### Configuration

IP Blocklist is configured through System Settings:

```json
{
  "blocked_ips": ["203.0.113.50", "198.51.100.0/24"]
}
```

### Behavior

- Requests from blocked IPs receive a 403 Forbidden response.
- The blocklist applies to all external API requests.
- Changes take effect immediately through the settings UI.

## API Key IP Restriction (Per-Key Allowlist)

API Key IP Restriction allows you to restrict individual API keys to only accept requests from specified source IP addresses or CIDR ranges. This is useful for limiting API key usage to specific servers or networks.

### How It Works

When an API key has `allowed_ips` configured, AxonHub checks the source IP of every request made with that key. If the source IP does not match any entry in the allowlist, the request is rejected with a 403 Forbidden response.

### Source IP

The per-key allowlist currently checks the **direct TCP peer address** of the connection. Forwarded headers such as `X-Forwarded-For` and `X-Real-IP` are **not** trusted unconditionally.

When AxonHub is deployed behind a reverse proxy (e.g., Nginx, Cloudflare, AWS ALB), the peer address is the proxy's IP, not the end client's. Include the proxy's IP or CIDR in the key's `allowed_ips`; otherwise every request coming through the proxy is rejected.

Trusted-proxy semantics that recover the real client IP from forwarded headers are designed as a separate feature and are intentionally not part of the current allowlist behavior.

### Configuration

You can configure IP restrictions when creating or editing an API key:

1. Navigate to **API Keys** in the dashboard.
2. Click **Create API Key** or edit an existing key.
3. Enable the **IP Restriction** toggle.
4. Enter one or more IP addresses or CIDR ranges, separated by commas (e.g., `192.168.1.0/24, 10.0.0.5, 2001:db8::/32`).
5. Save the API key.

### Supported Formats

- **Single IP**: `203.0.113.10` or `2001:db8::10`
- **CIDR Range**: `203.0.113.0/24` or `2001:db8::/32`

Both IPv4 and IPv6 are supported.

### Example

The admin GraphQL mutation accepts the per-key allowlist through the `allowedIps` field. Create an API key with an IP restriction:

```graphql
mutation CreateAPIKeyWithIPRestriction($input: CreateAPIKeyInput!) {
  createAPIKey(input: $input) {
    id
    name
    allowedIps
  }
}
```

```json
{
  "input": {
    "name": "Production Server Key",
    "projectID": "gid://axonhub/Project/1",
    "allowedIps": ["10.0.0.0/8", "192.168.1.100"]
  }
}
```

A request from an allowed IP succeeds; a request from a non-allowed IP is rejected with `403 IP address is not allowed for this API key`.

For updates, `allowedIps: []` sets an empty list and `clearAllowedIps: true`
explicitly disables the restriction. The web console uses the explicit clear
operation when its IP restriction switch is turned off. Omitting both fields
leaves the current restriction unchanged.

`allowed_ips` is included in API-key backups. Backups created before this field
was introduced remain readable and restore an empty allowlist. The database
column is added by Ent auto-migration; deployments with
`disable_auto_migration: true` must apply the generated schema change through
their normal migration process before starting this version.

## Layered Security

These three features work together to provide defense in depth:

| Feature | Scope | Action | When to Use |
|---------|-------|--------|-------------|
| IP Access Control | Global | Allow only listed IPs | Lock down entire instance |
| IP Blocklist | Global | Block specific IPs | Block abusive IPs at runtime |
| API Key IP Restriction | Per-Key | Allow only listed IPs | Restrict individual API keys |

Requests are evaluated in order: IP Access Control → IP Blocklist → API Key Authentication → API Key IP Restriction.
