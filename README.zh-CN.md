<div align="center">
  <img src="webui/public/vite.svg" width="48" alt="Resin Logo" />
  <h1>Resin</h1>
  <p><strong>将大量的代理订阅转化为一个稳定、智能、可观测且支持会话保持的统一代理入口。</strong></p>
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

**Resin** 是一个专为接管海量节点设计的**高性能智能代理池网关**。

它用于在上层屏蔽底层代理节点的不稳定性，将分散节点聚合为一个支持 **“会话保持（粘性路由）”** 的统一代理入口。

## 💡 为什么选择 Resin？

- **海量接管**：轻松管理十万级规模的代理节点。高性能，原生支持高并发。
- **智能调度与熔断**：全自动的 **被动+主动** 健康探测、出口 IP 探测、延迟分析，精准剔除坏节点。采用 P2C 算法结合按域名的延迟加权评分，智能选择最优节点。
- **业务友好的粘性代理**：让同一业务账号优先绑定同一出口 IP，节点异常时自动切换同 IP 节点，在多数场景下减少业务波动。
- **多种接入方式**：同时支持 HTTP 正向代理、SOCKS5 正向代理与 URL 反向代理，适配不同客户端与集成形态。
- **多接入点管理**：可在 WebUI 热添加监听端口，并分别控制每个端口是否开放管理页面、HTTP 正向代理、HTTP 反向代理和 SOCKS5。
- **可观测性**：提供详细的性能指标与日志记录，快速掌控全局（可视化 Web 管理后台）。包括完整的结构化请求日志，支持按平台、账号、目标站点等维度查询与审计。
- **简单与强大兼得**：开箱即用的默认配置与深度自定义功能。无论你是只需几分钟跑通简单场景的个人使用者，还是需要高并发与高可用性的企业级团队，Resin 都能游刃有余。
- **跨订阅智能去重**：不同订阅中配置相同的节点自动合并，共享健康状态，避免重复探测。
- **热更新**：更新常用配置不用重启，更新订阅节点不断连。
- **状态持久化**：重启后仍可恢复节点健康数据、延迟统计与租约绑定，便于生产环境连续运行与故障恢复。
- **零侵入粘性接入**：支持从业务原有请求头（如 API Key）自动提取账号身份，在常见接入方式下可尽量减少代码改动。
- **订阅热更新**：节点订阅刷新时增量同步，不中断现有连接。
- **灵活的节点隔离**：通过 Platform 概念，按正则表达式、地区等规则筛选节点，为不同业务构建独立的代理池。


> [!TIP]
> 您可以把本文档与项目详细设计文档 [`DESIGN.md`](DESIGN.md) 丢给 AI，然后问它任何你感兴趣的问题！


![](doc/images/dashboard_zh-cn.png)

---

## 🔌 支持协议与订阅格式

### 订阅来源

- 远程订阅 URL：`http://` 或 `https://`。
- 本地订阅内容：在 UI/API 中直接粘贴订阅内容。

### 订阅内容格式

- sing-box JSON：`{"outbounds":[...]}` 或原始出站数组 `[...]`。
- Clash JSON/YAML：`{"proxies":[...]}` 或 YAML `proxies:`。
- URI 行格式（每行一个节点）：`vmess://`、`vless://`、`trojan://`、`ss://`、`hysteria2://`、`http://`、`https://`、`socks5://`、`socks5h://`。
  其中 `http://`、`https://`、`socks5://`、`socks5h://` 需使用 `scheme://[user:pass@]host:port` 形式（可选 `#tag`；`https` 额外支持 `sni`/`servername`/`peer` 与 `allowInsecure`/`insecure` 查询参数）。
- 纯 HTTP 代理行：`IP:PORT` 或 `IP:PORT:USER:PASS`（支持 IPv4 和 IPv6）。
- Base64 包裹的文本订阅（例如 URI 行或纯文本节点列表）。

### 支持的出站节点协议类型

- 对于 sing-box JSON/原始 outbounds：`socks`、`http`、`shadowsocks`、`vmess`、`trojan`、`wireguard`、`hysteria`、`vless`、`shadowtls`、`tuic`、`hysteria2`、`anytls`、`ssh`，以及 Resin 扩展的 `mieru`。
- 对于 Clash 转换：`ss`/`shadowsocks`、`socks`/`socks4`/`socks4a`/`socks5`、`http`、`vmess`、`vless`、`trojan`、`wireguard`/`wg`、`hysteria`、`hysteria2`/`hy2`、`tuic`、`anytls`、`ssh`、`mieru`。

Mieru 使用官方 Go 客户端，支持下面的 Clash/Mihomo 配置，可通过远程或本地订阅导入；也支持直接粘贴裸 YAML 节点列表：

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

`transport` 支持 `TCP`、`UDP`，握手和多路复用参数会传递给官方客户端；省略时分别使用 `TCP`、`HANDSHAKE_STANDARD`、`MULTIPLEXING_LOW`。目标 TCP、UDP 流量均可承载，因此外层使用 TCP 的 Mieru 节点也可以作为 WARP 前置。`outbounds` JSON 中的 `mieru` 为 Resin 自定义扩展，使用 `server_port`、`handshake_mode` 等下划线字段，不代表上游 sing-box 1.12 原生支持此类型。


## Cloudflare WARP 派生出口

WebUI「订阅管理」自动提供一个系统维护的 **WARP** 订阅，统一管理所有 WARP 派生出口。编辑该订阅可启停 WARP，并调整出口总上限，默认 **300**、范围 **1–1024**。调大后后台自动补建；调小后保留延迟较低的出口并释放多余隧道，以限制内存占用。禁用该订阅会关闭 WARP 隧道，原始节点继续可用。此系统订阅无需 URL，不提供删除或手动拉取操作。

Resin 在现有支持 UDP 的代理节点上叠加用户态 WARP 隧道。系统先为不同的可用父节点各尝试一条出口，再增加同一父节点的隧道，尝试填充配置上限。双栈模式在数量相同时优先 IPv6，并以 IPv4、IPv6 接近各半为目标；某个地址族不可用时，另一个地址族仍可补足容量，恢复后逐步替换多余地址族以重新平衡。实际数量取决于可用前置节点及去重结果。WARP 派生节点只归属统一的 WARP 订阅，同时保留父节点关联。注册请求和 WireGuard 数据都经过对应父节点；HTTP 等仅 TCP 节点无法承载 WARP，不会改用宿主机直接建立隧道。无需 TUN、root 或 `NET_ADMIN`；自编译须包含 `with_wireguard with_gvisor`，项目 Docker 和发布构建已包含。

在某个平台中禁用这些出口，可关闭该平台的「允许 WARP 订阅节点」选项；等效标签排除规则为 `!^WARP/`。需要仅使用 WARP 出口时，可使用正向规则 `^WARP/`。平台级排除不会关闭其他平台使用的 WARP 隧道。

订阅配置通过 `PATCH /api/v1/subscriptions/system-warp` 保存，例如：

```json
{"enabled": true, "max_nodes": 300}
```

| 运行时字段 | 默认值 | 含义 |
|---|---|---|
| `warp_stack` | `dual` | `ipv4`、`ipv6` 或 `dual`；双栈可同时建立 IPv4、IPv6 出口。 |
| `warp_exits_per_family` | `1` | 每个父节点每地址族的起始出口数，范围 1–8；为达到订阅上限可自动继续扩展。 |
| `warp_reconnect_attempts` | `3` | IP 去重时建立候选出口的最大尝试次数，范围 1–10。 |

表中高级参数仍在「系统配置」调整，支持 `PATCH /api/v1/system/config` 热更新。旧 API 的 `warp_enabled` 和 `warp_max_nodes` 保留兼容，分别对应 WARP 订阅的 `enabled` 与 `max_nodes`，总上限默认值为 300。派生出口按实际探测的公网 IP 去重（IPv4、IPv6 分别按完整地址比较）；发现重复时重新建立 WARP，达到次数上限仍重复时，同 IP 的 WARP 线路仅保留延迟最低者。Cloudflare 可能一直分配共享出口，重连不保证得到新 IP，也不保证出口数量翻倍。IPv6 出口还受父节点的 UDP 能力及 Cloudflare 网络条件影响。

出口在后台最多并行预热 4 条，同一个父节点同时只尝试一条，WARP 设备注册仍共享至少 12 秒的间隔。首次覆盖可用前置后，按地址族和线路的最近尝试顺序公平轮转，成功前置的新增出口不会一直排在失败或重复线路的首条出口之后。每次调度只建立一个候选；遇到失败或重复 IP 后先处理其他前置，至少等待 1 分钟再重试同一候选，去重尝试达到配置次数后等待 5 分钟。同一前置的同一地址族连续连接失败 3 次后暂停 30 分钟，IPv6 失败不会暂停其 IPv4 出口。Cloudflare 返回 HTTP 429 时，全部注册共享退避时间，优先遵循 `Retry-After`，不计作父节点的连接失败。关闭后重新启用订阅或重启会重置父节点暂停状态。

成功通过 WARP 验证的父节点及地址族会记录到缓存目录的 `warp-parents.json`，重启后在同等分配条件下优先尝试这些线路，同时继续覆盖其他可用父节点。记录只包含父节点哈希、地址族、延迟和成功时间，有效期 7 天，最多保留 4096 条。派生隧道与设备凭据只保存在内存中，重启后重新建立。每次重连使用独立节点标识，旧请求或探测结果不会覆盖新出口状态。

WARP 的刷新状态反映后台的实际维护进度：启动、维护中、维护停滞或已禁用。超过 90 秒没有维护进展会显示停滞；已满额、暂无可用前置或等待重试时，正常运行的维护任务仍持续更新检查时间。最近维护时间通过订阅 API 的 `last_maintenance` 返回，状态通过 `maintenance_status` 返回；节点新增和移除都会更新「上次更新」。VMess、VLESS、Trojan 的 WebSocket early data 保持可用，连接关闭或建立超时会释放等待读取，避免阻塞 WARP 维护。

`0 / 0` 表示健康出口数 / 已发布出口总数，验证失败的候选不计入；上限不是已建立的隧道数。节点数下方分别显示已发布的 IPv6、IPv4 数量，便于查看双栈分配。最近错误会记录前置名称、哈希、协议、Cloudflare 端点、验证阶段及 WireGuard 握手和包计数。`wg_handshake=none` 表示尚未观察到握手成功；`wg_handshake=complete` 表示应继续检查隧道内 TCP/TLS/HTTP 阶段，单凭超时不能判定具体网络故障。前置节点自身 TCP 健康不代表其 UDP 转发可用，也不能仅按 SS、SOCKS5 等协议名判断。IPv6 WARP 出口可以通过前置的 IPv4 外层连接承载，无需前置本身具备 IPv6 出口。`system-warp` 是运行时系统订阅，其启停与上限保存在系统配置中，不写入普通订阅表。

## 🚀 Quick Start

只需简单三步，即可将你的节点订阅转化为高可用代理池。

### 第一步：一键部署启动
推荐使用 Docker Compose 快速启动：

```yaml
# docker-compose.yml
services:
  resin:
    image: ghcr.io/guji08233/resin:latest
    container_name: resin
    restart: unless-stopped
    environment:
      RESIN_ADMIN_TOKEN: "admin123" # 修改为你的管理后台密码
      RESIN_PROXY_TOKEN: "my-token" # 修改为你的代理密码
      RESIN_LISTEN_ADDRESS: 0.0.0.0
      RESIN_PORT: 2260
    ports:
      - "2260:2260"
    volumes:
      - ./data/cache:/var/cache/resin
      - ./data/state:/var/lib/resin
      - ./data/log:/var/log/resin
```
向本仓库的 `master` 分支推送后，GitHub Actions 会自动构建并发布 `ghcr.io/guji08233/resin` 的 `latest`、`master` 和 `sha-*` 镜像。使用本分支的 WARP、Mieru 功能前，请先拉取新镜像并重建容器：

```bash
docker compose pull
docker compose up -d
```

自定义接入点端口还必须能从容器外访问。Docker 无法为已运行容器动态增加端口映射，因此请在 `ports` 中提前映射所需端口范围（例如 `"2300-2399:2300-2399"`），或在适合的环境中使用 host 网络。

*(如果你不想使用 Docker，请跳转文末查看 [其他部署方式](#其他部署方式))*

### 第二步：导入代理节点
1. 浏览器打开 `http://127.0.0.1:2260`（请替换为你的服务器 IP）。
2. 输入刚才设置的 `RESIN_ADMIN_TOKEN` 登录后台。
3. 在左侧菜单找到 **「订阅管理」**，添加你的节点订阅。
4. 稍等片刻，等待节点池刷新出你的节点。

### 第三步：开始你的代理请求
客户端接入方式参考接下来的章节。常见场景下，你可以按客户端能力选择 HTTP 正向代理、SOCKS5 正向代理或反向代理。

## 🟢 基础使用（非粘性代理）

### 简单接入代理
如果你只需要一个高性能、大容量、且会自动健康管理的通用代理池，Resin 开箱即用。
启动 Resin 服务后，你可以按客户端能力选择 HTTP 正向代理、SOCKS5 正向代理或反向代理接入。  
如果你不想设置代理密码，请将环境变量显式设为空字符串：`RESIN_PROXY_TOKEN=""`（变量必须定义）。此时 HTTP 正向代理可直接接入 `http://127.0.0.1:2260`，SOCKS5 正向代理可直接接入 `socks5://127.0.0.1:2260`。
通过二进制文件或源码运行时，Resin 也会在读取配置前自动加载当前工作目录下的 `.env` 文件；已经由系统或 shell 设置的环境变量优先级高于 `.env`。

HTTP 正向代理例子：


```bash
curl -x http://127.0.0.1:2260 \
  -U ":my-token" \
  https://api.ipify.org
```

SOCKS5 正向代理例子：

```bash
curl --proxy socks5h://127.0.0.1:2260 \
  -U "Default:my-token" \
  https://api.ipify.org
```

当 `RESIN_PROXY_TOKEN=""` 时，SOCKS5 也允许无认证接入。

如果你的客户端支持修改服务的 `BASE_URL`，你也可以尝试反向代理接入。URL 格式为：`/令牌/Platform(可选).Account(可选)/协议/目标地址`。例如，你可以通过下面的 curl 命令通过 Resin 访问 `https://api.ipify.org`。

```bash
curl http://127.0.0.1:2260/my-token/./https/api.ipify.org
```

> 正向代理与反向代理的选择：如果条件允许，推荐尽量使用反向代理，对于可观测性更友好。如果您的客户端不支持修改 BaseURL，或者客户端需要 utls、非纯 WebAPI 请求这种反向代理不擅长的情况，请使用正向代理。

### 筛选节点
如果你的服务对节点有筛选要求，例如只需要某个地区的节点，或者只需要来自某个订阅源的节点，或者只需要名字匹配特定正则表达式的节点，可以使用 Resin 的 Platform 概念来实现。

你可以打开 `http://127.0.0.1:2260/ui/platforms` Platform 管理页面，创建一个 Platform。例如希望只使用来自美国、香港的节点，你可以创建一个名为 “MyPlatform” 的 Platform，然后在地区过滤规则中填入：
```
us
hk
```

节点标签规则匹配 `<订阅名>/<节点标签>`，每行是一条正则表达式：普通规则之间为 OR，`*` 开头表示必须命中，`!` 开头表示命中即排除。例如下面表示选择香港或日本的专线节点，同时排除带有“过期”或者“失效”标签的节点：

```text
香港
日本
*专线
!过期
!失效
```

规则前缀只识别每行的第一个字符，其余内容仍是 Go 正则表达式。需要匹配字面量行首 `!` 时可以写成 `\!`。

对于正向代理（HTTP / SOCKS5），你可以在认证信息中填入希望使用的 Platform。下面分别给出一个 curl 例子：

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

对于反向代理，你可以在 URL 前缀中提供 Platform 信息。下面是一个使用 curl 的例子：

```
curl http://127.0.0.1:2260/my-token/MyPlatform/https/api.ipify.org
```

## 📖 进阶使用：粘性代理

当业务遇到**对 IP 变化敏感**的服务，或者需要持续交互时，你需要使用 Resin 的核心特性：**粘性代理**。

在此之前，先了解两个概念：

### 🎯 核心概念：平台 (Platform) 与 账号 (Account)
- **平台 (Platform)**：节点的隔离池。你可以通过规则筛选节点（例如只使用“美国”节点）组建成一个专有池。系统默认存在一个装载所有可用节点的 `Default` 平台。
- **账号 (Account)**：业务侧的唯一标识（如 `Tom` 或 `user_1`）。携带特定 Account 的请求，Resin 会优先为其分配稳定的出口节点；当节点不可用时，会重试并优先切换到同 IP 节点，以降低业务侧适配成本。

### 粘性代理接入格式

#### 方式一：正向代理接入（HTTP Proxy / SOCKS5）
HTTP 正向代理与 SOCKS5 正向代理共用同一套身份格式：`Platform.Account:RESIN_PROXY_TOKEN`。

直接将身份信息写入代理认证中即可：

```bash
# HTTP 正向代理：指定业务账号 user_tom，Resin 会为其长期分配一个稳定的专属 IP
curl -x http://127.0.0.1:2260 \
  -U "Default.user_tom:my-token" \
  https://api.ipify.org
```

```bash
# SOCKS5 正向代理：身份格式与 HTTP 正向代理保持一致
curl --proxy socks5h://127.0.0.1:2260 \
  -U "Default.user_tom:my-token" \
  https://api.ipify.org
```

#### 方式二：反向代理接入（URL 携带 Account，适合简单使用/手动调试）
你可以通过替换业务的 BaseURL 为 Resin 反代地址，将请求直接发给 Resin。
URL 格式进阶为：`http://部署IP:2260/密码/平台.账号/协议/目标地址`：

```bash
# 例如让 user_tom 访问 https 协议的 cloudflare.com：
curl "http://127.0.0.1:2260/my-token/Default.user_tom/https/api.ipify.org"
```

> URL 中携带 Account 的模式定位是“简单使用 / 手动调试”。
> 生产环境长期集成，推荐优先使用请求头 `X-Resin-Account` 传递 Account。

#### 方式三：反向代理接入 + `X-Resin-Account` 请求头（推荐正式集成）

如果你的客户端（或 SDK）支持自定义请求头，建议直接使用 `X-Resin-Account` 显式传递 Account，这是最稳定的方式。

Account 来源优先级：`X-Resin-Account` 请求头 > 反向代理 URL 中的 Account > 请求头提取规则。

示例：

```bash
curl "http://127.0.0.1:2260/my-token/MyPlatform/https/api.example.com/v1/orders" \
  -H "X-Resin-Account: user_tom"
```

#### 方式四：反向代理接入 + 请求头规则（零侵入/低侵入集成）
如果你的客户端不方便设置 `X-Resin-Account`，但业务请求本身已经有稳定身份头（例如发给目标网站的 API Key、Token、Cookie 等），Resin 也可以通过“请求头提取规则”自动提取 Account。

假设你的服务本来每次请求目标 API 时，都会携带 `Authorization` 请求头：

1. 在管理页面修改 Platform 的配置，把 “反向代理账号为空行为” 修改为 “提取指定请求头作为 Account”。
2. 在 “用于提取 Account 的 Headers” 输入 `Authorization`。

此时，就算你在反向代理 URL 里不填 `Account`，Resin 也会在流量经过时读取并解析该 Header。例如：

```bash
curl "http://127.0.0.1:2260/my-token/MyPlatform/https/api.example.com/v1/orders" \
  -H "Authorization: sk-abc123"
```

上面的请求中，Resin 发现 sk-abc123 后，会将其作为 Account。后续只要带着同一把 API Key 的请求，会优先保持在同一个出口 IP 上。

> [!TIP]
> 除了 Platform 请求头配置外，Resin 还支持更高级的根据 URL 前缀决定请求头的高级功能！尝试把当前文档与 [DESIGN.md](DESIGN.md) 扔给 AI，问它 “请使用简单易懂的语言，向我介绍 Resin 指定请求头提取规则的两种方式，尤其是根据 URL 前缀决定请求头的方式。”

> 请仅在具备合法处理依据（如用户授权、合同约定或其他适用法律基础）时启用请求头提取，并确保你的日志留存与访问控制策略符合所在地法律法规及目标服务条款。

---

## 🤖 接入第三方项目

各类第三方客户端使用 Resin 的方式有所不同，对于业务代码的侵入程度也不同，总结如下：

💡 **如果你不需要粘性代理**

| 接入方式 | 代码侵入程度 | 说明 |
| :--- | :--- | :--- |
| 接入正向代理 | 🟢 **零侵入** | 客户端填入 HTTP 或 SOCKS5 代理地址及认证信息即可。 |
| 接入反向代理 | 🟢 **零/低侵入** | 修改服务 BaseURL 即可接入，适配极易。 |

💡 **如果你需要粘性代理**

| 接入方式 | 代码侵入程度 | 说明 |
| :--- | :--- | :--- |
| 接入正向代理 | 🟡 **中侵入** | 需稍微修改代码：为不同用户附带不同认证信息。HTTP 与 SOCKS5 都可使用 `平台.账号:密码`。 |
| 接入反向代理 | 🟡 **中侵入** | 需稍微修改代码：加入 `X-Resin-Account` 请求头或动态拼接带有账号的反代 URL 路径。 |
| 接入反向代理 + 请求头规则 | 🟢 **零/低侵入** | Resin 允许通过识别业务原始头（如 `Authorization`）自动提取 Account 并进行粘性路由绑定，接入方式与非粘性反代接近。 |

👉 **极速集成脚本/提示词（Prompt）：**  
如果你是开发者，想要修改现有项目原生接入 Resin 粘性代理，你可以直接把下面这个模板喂给 AI 帮你写代码：
- [doc/integration-prompt.md](doc/integration-prompt.md)

---

## 其他部署方式

<details>
<summary><b>方式一：运行预编译二进制文件</b></summary>
<br>
前往项目的 <a href="https://github.com/GuJi08233/Resin/releases">Release</a> 页面，下载适合你操作系统架构的程序包。解压得到单个二进制文件 <code>resin</code>。

```bash
RESIN_ADMIN_TOKEN=【管理面板密码】 \
RESIN_PROXY_TOKEN=【代理密码】 \
RESIN_STATE_DIR=./data/state \
RESIN_CACHE_DIR=./data/cache \
RESIN_LOG_DIR=./data/log \
RESIN_LISTEN_ADDRESS=0.0.0.0 \
RESIN_PORT=2260 \
./resin
```

也可以在工作目录创建 `.env` 文件，然后直接运行 `./resin`：

```dotenv
RESIN_ADMIN_TOKEN=【管理面板密码】
RESIN_PROXY_TOKEN=【代理密码】
RESIN_STATE_DIR=./data/state
RESIN_CACHE_DIR=./data/cache
RESIN_LOG_DIR=./data/log
RESIN_LISTEN_ADDRESS=0.0.0.0
RESIN_PORT=2260
```
</details>

<details>
<summary><b>方式二：通过源码编译</b></summary>
<br>
前提条件：请确保环境中已安装 Go 1.25+ 和 Node.js。

```bash
# 1. 下载 Resin 源码
git clone https://github.com/GuJi08233/Resin.git

# 2. 编译 WebUI
cd Resin/webui
npm install && npm run build
cd ..

# 3. 编译 resin 核心
go build -tags "with_quic with_wireguard with_gvisor with_grpc with_utls" -o resin ./cmd/resin

# 4. 运行程序
RESIN_ADMIN_TOKEN=【管理面板密码】 \
RESIN_PROXY_TOKEN=【代理密码】 \
RESIN_STATE_DIR=./data/state \
RESIN_CACHE_DIR=./data/cache \
RESIN_LOG_DIR=./data/log \
RESIN_LISTEN_ADDRESS=127.0.0.1 \
RESIN_PORT=2260 \
./resin
```
</details>

---

## 🛠️ 常见错误 (FAQ)

- **Q: 如何让内网或本机目标不走代理节点？**
  - **A**: 配置 `RESIN_PROXY_BYPASS`，用分号、逗号或换行分隔规则。命中的请求会由 Resin 本机直连目标，而不是通过代理节点。例如：`RESIN_PROXY_BYPASS="localhost;127.*;10.*;172.16.0.0/12;192.168.*;<local>"`。规则支持精确主机、`*`/`?` 通配符、CIDR 网段，以及表示无点号本地域名的 `<local>`。
- **Q: 启动失败提示 `RESIN_PROXY_TOKEN` 未定义？**
  - **A**: 就算你不打算启用代理密码，也必须显式配置它为空：`RESIN_PROXY_TOKEN=""`。如果你的 shell 会丢弃空环境变量，请创建 `.env` 文件并写入 `RESIN_PROXY_TOKEN=`。
- **Q: 为什么配置 `RESIN_AUTH_VERSION=LEGACY_V0` 会启动失败？**
  - **A**: 当前版本已不再支持 `LEGACY_V0`，请删除 `RESIN_AUTH_VERSION` 或将其设为 `V1`。如果你正从使用旧认证格式的版本升级，请参阅 [v1.0.0 认证迁移指南](doc/v1.0.0-migration-guide.zh-CN.md)。
- **Q: 为什么 SOCKS5 客户端连不上？**
  - **A**: 若 `RESIN_PROXY_TOKEN` 非空，客户端需要发送 SOCKS5 用户名密码认证；若它被显式设为空字符串，则也允许 `NO AUTH`。
- **Q: 使用反向代理 WebSocket 协议（如 ws/wss）怎么写路径？**
  - **A**: 目标无论是不是 ws/wss，URL 路径里的协议字段**依然只能写 `http` 或 `https`**（不能写 ws/wss）。Resin 会自动探测并完成 WebSocket 协议升级（Upgrade）。

---

## ⚠️ 免责声明与许可证

- **许可证**：本项目采用 [MIT License](LICENSE)。
- **使用性质**：本项目用于网络代理调度与管理的技术研究及工程实践，不构成任何法律、合规、审计或安全建议。
- **合法使用要求**：你必须自行确保使用行为符合所在地法律法规、目标服务条款（ToS）及数据处理要求，并确认你对代理节点、目标资源和相关数据具有合法授权。
- **禁止用途**：不得将本项目用于未授权访问、规避控制措施、欺诈、攻击、滥发请求或其他违法违规活动。
- **无担保条款**：本项目按“现状（AS IS）”提供，不附带任何明示或默示担保（包括但不限于适销性、特定用途适用性、非侵权）。
- **责任限制**：在适用法律允许的最大范围内，作者与贡献者不对因使用或无法使用本项目导致的任何直接、间接、附带或衍生损失承担责任。
