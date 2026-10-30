[English](README.md) | [简体中文](README.zh-CN.md)

<div align="center">
  <img src="webui/public/vite.svg" width="48" alt="Resin Logo" />
  <h1>Resin</h1>
  <p><strong>Turn massive proxy subscriptions into a stable, smart, observable unified proxy entrypoint with sticky sessions.</strong></p>
</div>

<p align="center">
  <a href="https://github.com/GuJi08233/Resin/releases"><img src="https://img.shields.io/github/v/release/GuJi08233/Resin?style=flat-square&label=release&sort=semver" alt="Release" /></a>
  <a href="https://github.com/Resinat/Resin/actions/workflows/release.yml"><img src="https://img.shields.io/github/actions/workflow/status/Resinat/Resin/release.yml?style=flat-square&label=release%20pipeline" alt="Release Pipeline" /></a>
  <a href="https://github.com/GuJi08233/Resin/pkgs/container/resin"><img src="https://img.shields.io/badge/ghcr-ghcr.io%2Fguji08233%2Fresin-2496ED?style=flat-square&logo=docker&logoColor=white" alt="GHCR Image" /></a>
  <a href="https://github.com/Resinat/Resin/blob/master/LICENSE"><img src="https://img.shields.io/github/license/Resinat/Resin?style=flat-square" alt="License" /></a>
  <a href="https://github.com/Resinat/Resin/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/Resinat/Resin?style=flat-square" alt="Go Version" /></a>
  <a href="https://github.com/GuJi08233/Resin/releases"><img src="https://img.shields.io/badge/support-linux%20%7C%20macOS%20%7C%20windows%20%C2%B7%20amd64%20%7C%20arm64-0A7EA4?style=flat-square" alt="Support Matrix" /></a>
  <a href="DESIGN.md"><img src="https://img.shields.io/badge/docs-DESIGN.md-1F6FEB?style=flat-square" alt="Design Docs" /></a>
</p>

---

**Resin** is a **high-performance intelligent proxy pool gateway** built for operating massive numbers of proxy nodes.

It helps shield your services from unstable underlying proxy nodes by aggregating distributed proxy resources into a unified proxy entrypoint with **session stickiness (sticky routing)**.

## 💡 Why Resin?

- **Massive-scale management**: Easily handles 100k+ proxy nodes with native high-concurrency performance.
- **Smart scheduling and circuit breaking**: Fully automated **passive + active** health checks, outbound IP probing, and latency analysis to remove bad nodes precisely. Uses P2C plus domain-aware latency-weighted scoring for optimal node selection.
- **Business-friendly sticky proxying**: Keeps the same business account bound to a stable outbound IP. If a node fails, Resin seamlessly switches to another node with the same IP.
- **Multiple access modes**: Supports HTTP forward proxy, SOCKS5 forward proxy, and URL-based reverse proxy for different clients and integration styles.
- **Multiple inbound endpoints**: Add hot-reloaded listening ports in the WebUI and independently control admin-console, HTTP forward, HTTP reverse, and SOCKS5 access on each port.
- **Observability**: Detailed metrics and logs, plus a visual Web UI. Includes complete structured request logs for querying and auditing by platform, account, target site, and more.
- **Simple and powerful**: Works out of the box with default settings, while still offering deep customization for enterprise-grade needs.
- **Cross-subscription deduplication**: Automatically merges identical nodes from different subscriptions and shares their health state.
- **Hot reload**: Update common settings without restart. Refresh subscriptions without dropping existing traffic.
- **Persistent state**: Keeps node health, latency stats, and lease bindings across restarts.
- **Zero-intrusion sticky access**: Can extract account identity from existing request headers (for example API keys), so clients often need no code changes.
- **Incremental subscription refresh**: Syncs subscription updates without interrupting current connections.
- **Flexible node isolation**: Use Platform rules (regex, region, etc.) to build independent proxy pools for different business scenarios.

> [!TIP]
> You can feed this README and [`DESIGN.md`](DESIGN.md) to AI and ask it anything about the project.

![](doc/images/dashboard_en-us.png)

---

## 🔌 Supported Protocols and Subscription Formats

### Subscription sources

- Remote subscription URL: `http://` or `https://`.
- Local subscription content: paste subscription content directly in the UI/API.

### Subscription content formats

- sing-box JSON: `{"outbounds":[...]}` or raw outbound array `[...]`.
- Clash JSON/YAML: `{"proxies":[...]}` or YAML `proxies:`.
- URI line format (one node per line): `vmess://`, `vless://`, `trojan://`, `ss://`, `hysteria2://`, `http://`, `https://`, `socks5://`, `socks5h://`.
  For `http://`, `https://`, `socks5://`, `socks5h://`, use `scheme://[user:pass@]host:port` (optional `#tag`; `https` also supports `sni`/`servername`/`peer` and `allowInsecure`/`insecure` query parameters).
- Plain HTTP proxy lines: `IP:PORT` or `IP:PORT:USER:PASS` (IPv4 and IPv6).
- Base64-wrapped text subscriptions (for URI lines/plain-text node lists).

### Supported outbound node types

- For sing-box JSON/raw outbounds: `socks`, `http`, `shadowsocks`, `vmess`, `trojan`, `wireguard`, `hysteria`, `vless`, `shadowtls`, `tuic`, `hysteria2`, `anytls`, `ssh`, and Resin's `mieru` extension.
- For Clash conversion: `ss`/`shadowsocks`, `socks`/`socks4`/`socks4a`/`socks5`, `http`, `vmess`, `vless`, `trojan`, `wireguard`/`wg`, `hysteria`, `hysteria2`/`hy2`, `tuic`, `anytls`, `ssh`, `mieru`.

Mieru uses the official Go client. Import Clash/Mihomo YAML or JSON subscriptions, or paste a bare YAML node list:

```yaml
proxies:
  - name: Mieru example
    type: mieru
    server: mieru.example.com
    port: 2012
    transport: TCP
    handshake-mode: HANDSHAKE_NO_WAIT
    username: example-user
    password: example-password
    multiplexing: MULTIPLEXING_HIGH
```

Both `TCP` and `UDP` outer transports are supported. Handshake and multiplexing settings are passed to the official client; omitted settings default to `TCP`, `HANDSHAKE_STANDARD`, and `MULTIPLEXING_LOW`, respectively. Mieru carries TCP and UDP destinations, so a TCP-transport Mieru node can also serve as a WARP parent. The `mieru` type in `outbounds` JSON is a Resin extension, using snake-case fields such as `server_port` and `handshake_mode`; upstream sing-box 1.12 does not natively implement it.

## Derived Cloudflare WARP Exits

The WebUI automatically provides a system-managed **WARP** entry under **Subscriptions** for all derived WARP exits. Edit it to enable or disable WARP and adjust the total exit limit: **300** by default, range **1–1024**. Raising the limit creates more exits in the background; lowering it keeps lower-latency exits and releases surplus tunnels to limit memory use. Disabling closes the WARP tunnels while original nodes remain available. This subscription needs no URL and has no delete or manual-fetch action.

WARP tunnels run over existing UDP-capable proxy nodes. Recovery first attempts one exit for each available parent before adding more tunnels per parent toward the configured limit. Dual stack prefers IPv6 on ties and aims for roughly equal IPv4 and IPv6 capacity. If one family is unavailable, the other can fill the pool; recovered families gradually replace surplus exits to restore balance. Actual capacity depends on available parents and deduplication results. Derived exits belong only to the unified WARP subscription while retaining their parent relationship. Both registration requests and WireGuard packets use that parent. TCP-only nodes cannot carry WARP, and there is no direct-host fallback. No TUN device, root access, or `NET_ADMIN` is required. Custom builds need `with_wireguard with_gvisor`, which are already enabled in the Docker and release builds.

To exclude these exits from one platform, turn off **Allow WARP subscription nodes** in that platform's settings; the equivalent exclusion rule is `!^WARP/`. Use the positive rule `^WARP/` to select only WARP exits. Platform exclusions do not close tunnels used by other platforms.

Subscription settings are persisted through `PATCH /api/v1/subscriptions/system-warp`, for example:

```json
{"enabled": true, "max_nodes": 300}
```

| Runtime field | Default | Description |
|---|---|---|
| `warp_stack` | `dual` | `ipv4`, `ipv6`, or `dual`; dual stack creates both IPv4 and IPv6 exits. |
| `warp_exits_per_family` | `1` | Initial exits per parent per family, 1–8; additional tunnels can be created to reach the subscription limit. |
| `warp_reconnect_attempts` | `3` | Maximum candidate creation attempts when deduplicating IPs, 1–10. |

The advanced fields in the table remain under **System Configuration** and support hot updates through `PATCH /api/v1/system/config`. The legacy API fields `warp_enabled` and `warp_max_nodes` remain compatible aliases for the WARP subscription's `enabled` and `max_nodes`, with the total limit defaulting to 300. Derived exits are deduplicated by their probed public IP, comparing full IPv4 and IPv6 addresses. Duplicate addresses trigger reconnects; after the attempt limit, only the lowest-latency WARP route for the same IP is kept. Cloudflare may keep assigning shared exits, so reconnecting does not guarantee a new IP or twice as many exits. IPv6 availability also depends on the parent proxy's UDP support and Cloudflare network conditions.

Up to four exits warm up concurrently, with at most one attempt per parent at a time. Device registrations still share a minimum 12-second interval. After initial parent coverage, address families and lanes rotate by their last attempt, so a working parent's additional exits cannot remain behind repeated first-slot failures or duplicates. Each scheduling turn creates only one candidate; failed or duplicate candidates yield to other parents before retrying after at least one minute. Reaching the configured duplicate retry count extends that delay to five minutes. Three consecutive connection failures pause only that parent's affected address family for 30 minutes; each further exhausted streak after a pause doubles it (1, 2, 4 hours) up to 6 hours, and one success resets it. Before registering a device, Resin sends one DNS query to `1.1.1.1:53` through the parent to confirm UDP relaying, without using the registration interval. A parent that explicitly refuses UDP (for example SOCKS5 UDP ASSOCIATE replying `code=7`) is paused for both IPv4 and IPv6 at once; an unanswered query counts as a connection failure for all of that parent's families. During the tunnel phase, repeated refusals of the outer UDP association end the attempt immediately instead of waiting for every remaining endpoint to time out. Registration queueing does not consume the 45-second tunnel budget, and a device whose tunnel failed is handed to the next attempt within 30 minutes (at most three uses per device) instead of registering again. Cloudflare HTTP 429 responses pause registration globally, honoring `Retry-After` when supplied, without counting as parent connection failures. Disabling and re-enabling the subscription, or restarting Resin, resets parent pauses.

Verified parent and address-family successes are recorded in `warp-parents.json` under the cache directory. After restart, these paths take priority within the same allocation tier while other available parents still receive coverage. Records contain only the parent hash, family, latency, and success time; they expire after seven days and are limited to 4096 entries. Tunnels and device credentials stay in memory and are recreated after restart. Each tunnel incarnation has a distinct node identity so old requests and probes cannot overwrite its replacement's state.

WARP status reflects actual background maintenance progress: starting, maintaining, stalled, or disabled. A worker with no progress for over 90 seconds is reported as stalled; each attempt reports progress after its UDP probe, registration, and every endpoint probe. A healthy worker keeps updating its check time even when the pool is full, no eligible parents are available, or retries are cooling down. The subscription API exposes `maintenance_status` and `last_maintenance`; both additions and removals update `last_updated`. WebSocket early data remains supported for VMess, VLESS, and Trojan, with connection close and setup timeouts releasing pending readers so they cannot block WARP maintenance.

`0 / 0` means healthy exits / published exits; failed candidates are not counted, and the limit is not an established tunnel count. Published IPv6 and IPv4 counts appear below the node count so the dual-stack allocation is visible. The latest error identifies the parent name, hash and protocol, Cloudflare endpoint, verification phase, WireGuard handshake state and packet counters. `wg_handshake=none` means no successful handshake was observed; `wg_handshake=complete` points diagnosis toward the inner TCP/TLS/HTTP path. A timeout alone does not locate the network fault. A parent's TCP health does not prove UDP forwarding works, and the protocol name alone does not establish reachability. An IPv6 WARP exit can use an IPv4 outer connection, so its parent does not need a native IPv6 exit. `system-warp` is a runtime-managed subscription whose settings are stored in system configuration, not in the ordinary subscriptions table.

## 🚀 Quick Start

In just three steps, you can turn your proxy subscriptions into a highly available proxy pool.

### Step 1: Deploy and start

Docker Compose is the recommended quick-start path:

```yaml
# docker-compose.yml
services:
  resin:
    image: ghcr.io/guji08233/resin:latest
    container_name: resin
    restart: unless-stopped
    environment:
      RESIN_ADMIN_TOKEN: "admin123" # Change to your admin dashboard password
      RESIN_PROXY_TOKEN: "my-token" # Change to your proxy password
      RESIN_LISTEN_ADDRESS: 0.0.0.0
      RESIN_PORT: 2260
    ports:
      - "2260:2260"
    volumes:
      - ./data/cache:/var/cache/resin
      - ./data/state:/var/lib/resin
      - ./data/log:/var/log/resin
```

Pushes to this repository's `master` branch automatically build and publish `latest`, `master`, and `sha-*` images at `ghcr.io/guji08233/resin`. To use the WARP and Mieru features in this branch, pull the new image and recreate the container:

```bash
docker compose pull
docker compose up -d
```

Custom endpoint ports must also be reachable from outside the container. Docker cannot add published ports to an already-running container, so pre-publish the required port range in `ports` (for example `"2300-2399:2300-2399"`) or use host networking where appropriate.

*(If you don't want Docker, jump to [Other Deployment Options](#other-deployment-options).)*

### Step 2: Import proxy nodes

1. Open `http://127.0.0.1:2260` in your browser (replace with your server IP if needed).
2. Log in with the `RESIN_ADMIN_TOKEN` you set.
3. Go to **Subscriptions** in the left menu and add your node subscription.
4. Wait briefly for the node pool to refresh.

### Step 3: Start sending proxy requests

See the following sections for client access modes. In common scenarios, you can choose HTTP forward proxy, SOCKS5 forward proxy, or reverse proxy based on what your client supports.

## 🟢 Basic Usage (Non-sticky Proxy)

### Connect as a standard proxy

If you just need a high-performance, large-capacity proxy pool with automatic health management, Resin works out of the box.

Once Resin is running, you can choose HTTP forward proxy, SOCKS5 forward proxy, or reverse proxy based on what your client supports.
If you do not want a proxy password, explicitly set `RESIN_PROXY_TOKEN=""` (the variable must still be defined). Then HTTP forward proxy is available at `http://127.0.0.1:2260`, and SOCKS5 forward proxy is available at `socks5://127.0.0.1:2260`.
For binary/source runs, Resin also loads a `.env` file from the current working directory before reading configuration. Environment variables already set by the OS or shell take precedence over `.env` values.

HTTP forward proxy example:

```bash
curl -x http://127.0.0.1:2260 \
  -U ":my-token" \
  https://api.ipify.org
```

SOCKS5 forward proxy example:

```bash
curl --proxy socks5h://127.0.0.1:2260 \
  -U "Default:my-token" \
  https://api.ipify.org
```

When `RESIN_PROXY_TOKEN=""`, SOCKS5 also allows unauthenticated access.

If your client supports overriding `BASE_URL`, you can also use reverse-proxy mode.
URL format: `/token/Platform(optional).Account(optional)/protocol/target`.
Example request to `https://api.ipify.org`:

```bash
curl http://127.0.0.1:2260/my-token/./https/api.ipify.org
```

> Choosing forward vs reverse proxy: when possible, reverse proxy is recommended for better observability. If your client cannot change BaseURL, or requires scenarios better served by forward proxy (such as uTLS or non-WebAPI traffic), use forward proxy.

### Filter nodes

If your service needs specific nodes (for example by region, subscription source, or name regex), use Resin's Platform feature.

Open `http://127.0.0.1:2260/ui/platforms` and create a Platform. For example, to use only US and HK nodes, create `MyPlatform` and set region filters to:

```
us
hk
```

Node tag rules match `<SubscriptionName>/<NodeTag>`, with one regular expression per line. Plain rules are ORed, a leading `*` marks a required rule, and a leading `!` excludes a node on any match. For example, this selects dedicated Hong Kong or Japan nodes while excluding expired and invalid nodes:

```text
Hong Kong
Japan
*Dedicated
!Expired
!Invalid
```

Only the first character is interpreted as a rule prefix; the rest remains a Go regular expression. Escape a literal leading `!` as `\!`.

For forward proxy (HTTP / SOCKS5), include Platform in the auth info. Examples:

```bash
curl -x http://127.0.0.1:2260 \
  -U "MyPlatform:my-token" \
  https://api.ipify.org
```

```bash
curl --proxy socks5h://127.0.0.1:2260 \
  -U "MyPlatform:my-token" \
  https://api.ipify.org
```

For reverse proxy, include Platform in the URL prefix:

```bash
curl http://127.0.0.1:2260/my-token/MyPlatform/https/api.ipify.org
```

## 📖 Advanced Usage: Sticky Session Proxy

When your business depends on IP continuity or long-lived interactions, use Resin's core feature: **sticky proxying**.

First, understand two core concepts:

### 🎯 Core Concepts: Platform and Account

- **Platform**: An isolated node pool. You can build it with filters (for example, only US nodes). Resin provides a default `Default` platform containing all available nodes.
- **Account**: A unique business identity (for example `Tom` or `user_1`). For requests carrying an Account, Resin anchors traffic to a dedicated high-quality outbound node. If that node fails, Resin retries seamlessly and switches to another node with the same IP.

### Sticky proxy access formats

#### Method 1: Forward proxy (HTTP Proxy / SOCKS5)

HTTP forward proxy and SOCKS5 forward proxy share the same identity format: `Platform.Account:RESIN_PROXY_TOKEN`.

Just put the identity in proxy authentication:

```bash
# HTTP forward proxy: bind business account user_tom to a stable dedicated outbound IP
curl -x http://127.0.0.1:2260 \
  -U "Default.user_tom:my-token" \
  https://api.ipify.org
```

```bash
# SOCKS5 forward proxy: uses the same identity format as HTTP forward proxy
curl --proxy socks5h://127.0.0.1:2260 \
  -U "Default.user_tom:my-token" \
  https://api.ipify.org
```

#### Method 2: Reverse proxy (URL Account, quick/manual debug)

By replacing your service BaseURL with Resin reverse-proxy URL, traffic goes through Resin directly.
Advanced URL format: `http://host:2260/token/platform.account/protocol/target`:

```bash
# Example: user_tom accesses api.ipify.org over https
curl "http://127.0.0.1:2260/my-token/Default.user_tom/https/api.ipify.org"
```

> The URL Account segment is designed for quick use and manual debugging.
> For long-running production integrations, prefer passing Account by header (`X-Resin-Account`).

#### Method 3: Reverse proxy + `X-Resin-Account` header (recommended production integration)

If your client/SDK supports custom request headers, pass Account explicitly with `X-Resin-Account`.
This is the recommended and most stable method.

Account source priority: `X-Resin-Account` header > Account in reverse-proxy URL > header extraction rules.

Example:

```bash
curl "http://127.0.0.1:2260/my-token/MyPlatform/https/api.example.com/v1/orders" \
  -H "X-Resin-Account: user_tom"
```

#### Method 4: Reverse proxy + header rules (zero/low-intrusion integration)

If your client cannot set `X-Resin-Account`, Resin can still extract Account from existing business headers (for example API Key, Token, Cookie) via header rules.

Assume your requests already include an `Authorization` header:

1. In Platform Configuration, set `Reverse-proxy empty-account behavior` to `Extract specified request headers as Account`.
2. Set `Headers used to extract Account` to `Authorization`.

Then even if Account is omitted in URL, Resin can still parse it from headers:

```bash
curl "http://127.0.0.1:2260/my-token/MyPlatform/https/api.example.com/v1/orders" \
  -H "Authorization: sk-abc123"
```

In this example, Resin uses `sk-abc123` as Account. Future requests with the same key are intended to stay bound to the same outbound IP whenever routing conditions allow.

> [!TIP]
> Beyond Platform header config, Resin also supports advanced rules that pick extraction headers by URL prefix. You can ask AI to explain both modes with this README and [DESIGN.md](DESIGN.md).

> Enable header-based Account extraction only when you have a valid legal basis (for example user authorization or contractual permission), and ensure your logging, retention, and access-control policies comply with applicable laws and target-service terms.

---

## 🤖 Integrating Third-party Projects

Different clients integrate Resin differently, with different code-intrusion levels.

💡 **If you do not need sticky proxying**

| Access Method | Code Intrusion | Notes |
| :--- | :--- | :--- |
| Forward proxy | 🟢 **Zero intrusion** | Just configure an HTTP or SOCKS5 proxy address plus credentials. |
| Reverse proxy | 🟢 **Zero/low intrusion** | Usually only requires changing service BaseURL. |

💡 **If you need sticky proxying**

| Access Method | Code Intrusion | Notes |
| :--- | :--- | :--- |
| Forward proxy | 🟡 **Medium intrusion** | Per-user requests need different auth info. Both HTTP and SOCKS5 use `platform.account:token`. |
| Reverse proxy | 🟡 **Medium intrusion** | Add `X-Resin-Account` request header or build reverse-proxy URL paths dynamically with account information. |
| Reverse proxy + header rules | 🟢 **Zero/low intrusion** | Resin can extract Account from original headers (for example `Authorization`) and bind IP automatically. |

👉 **Fast integration script/prompt:**
If you are a developer and want AI to help modify an existing project for native Resin sticky integration, use:

- [doc/integration-prompt.md](doc/integration-prompt.md)

---

## Other Deployment Options

<details>
<summary><b>Option 1: Run prebuilt binary</b></summary>
<br>
Go to the project's <a href="https://github.com/GuJi08233/Resin/releases">Release</a> page and download the package for your OS/architecture. After extraction, run the single binary <code>resin</code>.

```bash
RESIN_ADMIN_TOKEN=<admin-dashboard-password> \
RESIN_PROXY_TOKEN=<proxy-password> \
RESIN_STATE_DIR=./data/state \
RESIN_CACHE_DIR=./data/cache \
RESIN_LOG_DIR=./data/log \
RESIN_LISTEN_ADDRESS=0.0.0.0 \
RESIN_PORT=2260 \
./resin
```

Alternatively, create a `.env` file in the working directory and run `./resin`:

```dotenv
RESIN_ADMIN_TOKEN=<admin-dashboard-password>
RESIN_PROXY_TOKEN=<proxy-password>
RESIN_STATE_DIR=./data/state
RESIN_CACHE_DIR=./data/cache
RESIN_LOG_DIR=./data/log
RESIN_LISTEN_ADDRESS=0.0.0.0
RESIN_PORT=2260
```
</details>

<details>
<summary><b>Option 2: Build from source</b></summary>
<br>
Prerequisites: Go 1.25+ and Node.js installed.

```bash
# 1. Clone Resin source
git clone https://github.com/GuJi08233/Resin.git

# 2. Build WebUI
cd Resin/webui
npm install && npm run build
cd ..

# 3. Build Resin core
go build -tags "with_quic with_wireguard with_gvisor with_grpc with_utls" -o resin ./cmd/resin

# 4. Run
RESIN_ADMIN_TOKEN=<admin-dashboard-password> \
RESIN_PROXY_TOKEN=<proxy-password> \
RESIN_STATE_DIR=./data/state \
RESIN_CACHE_DIR=./data/cache \
RESIN_LOG_DIR=./data/log \
RESIN_LISTEN_ADDRESS=127.0.0.1 \
RESIN_PORT=2260 \
./resin
```
</details>

---

## 🛠️ FAQ

- **Q: How do I let LAN or localhost targets skip proxy nodes?**
  - **A**: Set `RESIN_PROXY_BYPASS` to a semicolon/comma/newline-separated rule list. Matching requests are dialed directly by Resin instead of through a proxy node. Example: `RESIN_PROXY_BYPASS="localhost;127.*;10.*;172.16.0.0/12;192.168.*;<local>"`. Supported rules include exact hosts, `*`/`?` wildcards, CIDR ranges, and `<local>` for hostnames without dots.
- **Q: Startup fails with `RESIN_PROXY_TOKEN` undefined?**
  - **A**: Even if you do not want a proxy password, you must explicitly set it to empty: `RESIN_PROXY_TOKEN=""`. On shells that drop empty environment variables, create a `.env` file with `RESIN_PROXY_TOKEN=`.
- **Q: Why does Resin reject `RESIN_AUTH_VERSION=LEGACY_V0`?**
  - **A**: `LEGACY_V0` is no longer supported. Remove `RESIN_AUTH_VERSION` or set it to `V1`. If you are upgrading from a release that used legacy authentication, see the [v1.0.0 auth migration guide](doc/v1.0.0-migration-guide.md).
- **Q: Why can't my SOCKS5 client connect?**
  - **A**: If `RESIN_PROXY_TOKEN` is non-empty, the client must send SOCKS5 username/password authentication. If it is explicitly set to an empty string, `NO AUTH` is also allowed.
- **Q: How to write reverse-proxy paths for WebSocket (ws/wss)?**
  - **A**: In the URL path, the protocol field must still be `http` or `https` (not `ws`/`wss`). Resin auto-detects and handles WebSocket upgrade.

---

## ⚠️ License and Disclaimer

- **License**: This project is licensed under the [MIT License](LICENSE).
- **Use scope**: This project is provided for technical research and engineering practice in proxy scheduling and management. It does not constitute legal, compliance, audit, or security advice.
- **Lawful use requirement**: You are responsible for ensuring your usage complies with applicable laws, regulations, target-service Terms of Service (ToS), and data-processing obligations, and for ensuring you have proper authorization for proxy nodes, target resources, and related data.
- **Prohibited use**: You must not use this project for unauthorized access, control-evasion, fraud, attacks, abusive traffic generation, or other illegal or non-compliant activity.
- **No warranty**: This project is provided on an "AS IS" basis, without express or implied warranties, including but not limited to merchantability, fitness for a particular purpose, and non-infringement.
- **Limitation of liability**: To the maximum extent permitted by applicable law, authors and contributors are not liable for any direct, indirect, incidental, special, exemplary, or consequential damages arising from use of, or inability to use, this project.
