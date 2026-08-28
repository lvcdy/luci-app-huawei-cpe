# 03 · 架构与接口定义

> 本文档确立 daemon 内部分层、LuCI↔daemon 通信协议、数据模型、事件/通知模型，
> 作为实现阶段的唯一接口契约。原则：**可靠性 > 简单性 > 可维护性 > 功能数量**。

## 0. 总体架构

```
┌────────────────────────── LuCI (Services → Huawei CPE) ──────────────────────────┐
│  Dashboard / SMS / Signal / Traffic / Notifications / Settings                  │
└───────────────▲───────────────────────────────────────────▲────────────────────┘
                │ HTTP (127.0.0.1:9090)                      │ HTTP
                └──────────────┬─────────────────────────────┘
                    ┌──────────▼───────────┐
                    │   huawei-cpe daemon  │  (procd 托管, /etc/init.d/huawei-cpe)
                    │                      │
                    │  ┌────────────────┐  │  内部服务层（事件驱动）
                    │  │  EventBus      │  │  sms.received / cpe.online /
                    │  │  (in-process)  │  │  cpe.offline / network.* / signal.changed
                    │  └──┬─────────┬───┘  │
                    │  ┌──▼──┐  ┌───▼───┐ │
                    │  │SMS  │  │Monitor│ │    ... Notification Manager 订阅事件
                    │  │Mgr  │  │Mgr    │ │
                    │  └──┬──┘  └───┬───┘ │
                    │  ┌──▼─────────▼───┐ │
                    │  │  DeviceManager │ │  会话持有者(1连接/设备) + 重连+身份+缓存
                    │  └───────┬────────┘ │
                    │          │          │
                    │  ┌───────▼────────┐ │  纯依赖，不修改
                    │  │ huawei-lte-api-go│ │  仅公开 API
                    │  └────────────────┘ │
                    └────────┬────────────┘
                             │ LAN
                    ┌────────▼─────────┐
                    │  Huawei LTE/5G CPE │
                    └──────────────────┘
```

- **LuCI 从不直接访问 CPE**；只访问 daemon 的 localhost HTTP API。
- **Notify（Telegram/Email/Webhook）从 daemon 出网**（OpenWrt 已有 WAN）。
- 存储：SQLite（信号历史、流量历史、短信、事件日志）。

## 1. 通信协议：LuCI ↔ daemon（HTTP on 127.0.0.1）

- 监听 `127.0.0.1:9090`（可配置），**仅回环**。
- JSON（`application/json`）。
- 无鉴权（回环 + 同机 LuCI 已鉴权；如未来需要，加回环 ACL / token）。
- 长耗时操作（重启 CPE 等）用 `POST /api/v1/devices/{id}/actions/reboot`，异步返回 202 + 轮询事件。
- 超时：daemon 侧 API 5s；CPE 请求超时默认 5s（连接+读写，SDK `NewConnection` 的 timeout 参数）。

### 1.1 端点清单（v0.1）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/status` | daemon 健康 + 汇总（所有设备简况） |
| GET | `/api/v1/devices` | 设备列表（含在线态、能力矩阵） |
| GET | `/api/v1/devices/{id}` | 设备详情（信息/网络/信号/流量/上下行速率） |
| POST | `/api/v1/devices/{id}/actions/discover` | 重新发现/探测设备 | 
| POST | `/api/v1/devices/{id}/actions/reboot` | 重启 CPE（202 异步） |
| GET | `/api/v1/devices/{id}/sms`  | 短信列表（SQLite，分页/筛选/搜索） |
| POST | `/api/v1/devices/{id}/sms/{sid}/read` | 标记已读（回写 CPE + SQLite） |
| DELETE | `/api/v1/devices/{id}/sms/{sid}` | 删除（回写 CPE + SQLite） |
| POST | `/api/v1/devices/{id}/sms` | 发送短信（可选，MVP 尾期） |
| GET | `/api/v1/devices/{id}/signal/history?bucket=h1\|d7\|d30` | 信号趋势数据（聚合） |
| GET | `/api/v1/devices/{id}/traffic/history?bucket=d1\|d7\|d30` | 流量趋势数据 |
| GET | `/api/v1/events?limit=50` | 最近事件（供 Notification 页） |
| GET | `/api/v1/notifications/status` | 各渠道配置摘要（不含秘密） |
| POST | `/api/v1/notifications/test` | 发送一条测试通知 |
| GET | `/api/v1/config` | 读 UCI 配置（脱敏） |
| PUT | `/api/v1/config` | 写 UCI 配置（`uci commit` + reload 信号） |

## 2. 进程内上下文与单元

```
huawei-cpe run
 ├── config      (UCI loader + watcher → SIGUSR1 reload)
 ├── httpapi     (127.0.0.1:9090, net/http, router: stdlib)
 ├── cache       (sync.Map 或小结构体; 状态快照, 最近值, 能力矩阵)
 ├── discover    (候选 IP 探测: 配置IP → LAN网关/子网 → 手动IP)
 ├── devicemgr   (设备实例管理; 每设备①Connection+重连策略②轮询循环③身份)
 ├── poller      (每个设备一个 goroutine; 依 polling_interval 采集)
 ├── signalhist  (SQLite 写入 + 聚合查询; 30d 保留, 可配)
 ├── traffichist (SQLite 写入 + 聚合)
 ├── smsmgr      (同步循环 30~60s; 去重; SetRead/Delete 回写; 事件发布)
 ├── eventbus    (topic→订阅者; 缓冲+非阻塞; 事件带时间戳与设备id)
 ├── notifymgr   (订阅 sms.received/cpe.online/cpe.offline→规则匹配→渠道)
 ├── notify/tg   (Telegram Bot; 白名单; /status /sms /help; 无写操作)
 ├── notify/smtp (Email; TLS; 主题前缀 [Huawei CPE])
 ├── notify/webhook (POST JSON, 限时, 失败不阻塞)
 ├── netmon      (每设备: CPE可达性 + 外网连通性; 状态机: online/offline/wan-down)
 ├── recovery    (可选: 自动恢复; 默认关; 防抖+冷却+最大次数)
 └── slog syslog (log/slog → syslog; 永不输出秘密)
```

## 3. 数据模型

### 3.1 UCI（`/etc/config/huawei_cpe`）

```
config cpe 'main'
    option enabled '1'
    option name '主路由'
    option host '192.168.8.1'
    option username 'admin'
    option password ''
    option polling_interval '60'     # 采集间隔(秒), 默认60, 下限10, 上限3600
    option sms_sync_interval '30'    # 短信同步间隔(秒), 默认30

config netmon 'health'
    option enabled '0'               # 网络健康监控；默认关? MVP: 开
    option ping_host '223.5.5.5'     # 外网探测目标(OpenWrt 侧 ping)
    option ping_interval '30'

config recovery 'recovery'
    option enabled '0'               # 自动恢复默认关
    option max_attempts '3'
    option cooldown_minutes '30'
    option reboot_on_fail '0'

config notify 'telegram'
    option enabled '0'
    option bot_token ''
    option allowed_chat_ids ''       # 逗号分隔

config notify 'smtp'
    option enabled '0'
    option host ''
    option port '465'
    option username ''
    option password ''
    option from ''
    option to ''                     # 逗号分隔
    option tls '1'                   # 或 starttls

config notify 'webhook'
    option enabled '0'
    option url ''

config rules 'sms_received'
    option telegram '1'
    option email '1'
    option webhook '1'
    option filter_mode 'all'         # all | whitelist | keyword
    option whitelist '10086,10010'   # 逗号分隔
    option keywords ''               # 逗号分隔
    option sms_forward_secret '0'    # 0=不转发(仅本地) 除非明确开启
```

### 3.2 设备模型（内存）

```go
type Device struct {
    ID        string // slug: "main" 或 host 哈希
    Name      string
    Host      string
    Username  string // 内存持有, 不落日志
    Password  string // 内存持有
    Online    bool
    LastSeen  time.Time
    Info      DeviceInfo  // Information() 快照
    Network   NetworkState
    Signal    SignalState
    Traffic   TrafficState
    Caps      Capabilities // 探测后缓存
    Mode      string  // "5G NSA" 等派生描述
}

type Capabilities struct {
    SMS    bool
    Signal bool
    Traffic bool
    Reboot bool
    CellInfo bool
    // 探测一次, 缓存; NotSupported → false
}
```

### 3.3 网络状态机 & 健康（区分 CPE 与 Internet）

```
CPE 可达(API 200 + 登录 OK) ──┬── Internet 通(OpenWrt ping 探测目标 OK)
                              │
    CPE: Online / Internet: Online    正常
    CPE: Online / Internet: Offline   WAN down（CPE 在但上不了网）
    CPE: Offline                      设备不可达（含超时/密码错/会话坏）
```

每个设备一个 `HealthState {CPEOnline bool; InternetOnline bool; LastCPECheck, LastInternetCheck time.Time}`。

### 3.4 SQLite（`/var/lib/huawei-cpe/huawei-cpe.db`）

```sql
PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS devices (
    id TEXT PRIMARY KEY, name TEXT, host TEXT, model TEXT, firmware TEXT,
    last_seen INTEGER, created_at INTEGER
);

CREATE TABLE IF NOT EXISTS signal_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL,
    ts INTEGER NOT NULL,             -- unix seconds
    rsrp INTEGER, rsrq INTEGER, sinr INTEGER, rssi INTEGER,
    lte_band TEXT, nr_band TEXT, pci INTEGER, cell_id INTEGER, earfcn INTEGER, nrarfcn INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sig_dev_ts ON signal_history(device_id, ts);

CREATE TABLE IF NOT EXISTS traffic_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT NOT NULL, ts INTEGER NOT NULL,
    rx_bytes INTEGER, tx_bytes INTEGER, rx_rate REAL, tx_rate REAL
);
CREATE INDEX IF NOT EXISTS idx_traff_dev_ts ON traffic_history(device_id, ts);

CREATE TABLE IF NOT EXISTS sms (
    id INTEGER PRIMARY KEY AUTOINCREMENT,        -- 本地自增
    device_id TEXT NOT NULL,
    cpe_index INTEGER NOT NULL,                  -- SDK Message.Index → 去重
    phone TEXT, content TEXT,
    status INTEGER,                              -- SmsStatus 0=new 1=read ...
    received_at INTEGER NOT NULL,
    read_local INTEGER DEFAULT 0,
    UNIQUE(device_id, cpe_index)
);
CREATE INDEX IF NOT EXISTS idx_sms_dev ON sms(device_id, received_at);

CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id TEXT, ts INTEGER NOT NULL,
    event TEXT NOT NULL, detail TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
```

- **去重**：`UNIQUE(device_id, cpe_index)`；冲突→忽略（不重复通知）。
- **保留期**：`signal_history` / `traffic_history` 默认 30 天（可配 `history_retention_days`），后台每天清理。
- SQLite 驱动选型：现代 `modernc.org/sqlite`（纯 Go，无 CGO，OpenWrt 交叉编译友好）→ **首选**；`mattn/go-sqlite3` 需 CGO，不是首选。（见 03 §依赖。）

## 4. 事件模型

```go
type Event struct {
    ID       string                 // uuid 或 nanos 计数
    Type     string                 // "sms.received" | "cpe.online" | "cpe.offline" | "network.connected" | "network.disconnected" | "signal.changed"
    DeviceID string
    At       time.Time
    Data     map[string]any         // 附加: sender/content（sms）、rsrp…（signal）
}
```

- `EventBus`：进程内，无缓冲通道或每订阅者缓冲队列；**notify 失败不阻塞采集循环**（独立 goroutine + 超时）。
- 事件也写入 `events` 表（供 Notification 页与未来审计）。

## 5. 通知模型

```
sms.received ──┬── filter (all|whitelist|keyword) ── 通过?
               │                                        │
               │        channel 启用? (telegram/email/webhook)
               └──────> rules 表 (sms_received: tg/email/webhook 勾选)
                        │
                        ▼
              NotificationManager.Send(ctx, chan, payload)
                        失败 → 记日志+事件; 不阻塞
```

通知文本（Telegram）：

```
Huawei H168-383
Status: Online
Network: 5G NSA
Operator: China Mobile
Band: B3 + n41
RSRP: -75 dBm
RSRQ: -8 dB
SINR: 22 dB
↓ 320 Mbps
↑ 80 Mbps
```

## 6. 采集与缓存节奏（防高频打 CPE）

| 数据 | 来源 | 周期 | 缓存 |
|---|---|---|---|
| Dashboard 状态 (monitoring/status) | CPE | 轮询循环内（60s） | 内存 5~10s 返回 LuCI |
| 设备信息 / 信号 / 网络 | CPE | 同上 | 内存 |
| 信号历史 SQLite | 轮询结果 | 60s（可配） | — |
| 流量 + 速率差分 | CPE | 60s | 内存 + SQLite |
| 短信同步 | CPE | 30~60s | SQLite + 内存最近 ID |
| 健康探测 | OpenWrt 侧 | CPE:60s / Internet:30s | 内存 |
| LuCI 页面刷新 | daemon 缓存 | 任意 | 5~10s 快照 |

- 单设备单轮询 goroutine，串行发请求，永不并行打 CPE。
- 任一采集失败：记日志，下一周期再试（指数退避，上限 5 分钟）。

## 7. 错误处理策略（daemon 层）

| SDK 错误 | 分类 | 动作 |
|---|---|---|
| `LoginInvalidCredentialsError` | 永久 | 停止该设备自动重试；事件 `cpe.auth_failed`；LuCI 显示「凭据错误」 |
| `LoginRequiredError` | 会话过期 | 重登（`User.Login`）→ 重试一次；仍失败 → 永久性退避 |
| `NotSupportedError` | 能力缺失 | 记入能力矩阵缓存；该 API 不再调用；页面显示 Not supported |
| `SystemBusyError` | 临时 | 退避重试（1s→5s→30s） |
| 网络超时 / 连接失败 | CPE 离线 | 标记 health 离线；指数退避；恢复后 `cpe.online` |
| XML/解码异常 | 临时/设备怪 | 记日志（含设备型号），退避重试，不崩溃 |

## 8. 安全基线（贯穿实现）

- 密码/token/SMTP 密码：仅存在于 `/etc/config/huawei_cpe` 与 daemon 内存；不写日志、不写 API 返回、不写错误链、LuCI 密码框不回显。
- Telegram bot token：不写日志；API 错误只记 `tg token 校验失败` 之类。
- 短信正文：默认仅本地 SQLite；转发必需用户明确开启（`sms_forward_secret=1` 且规则命中）。
- Webhook URL 若含 secret：日志打码。
- API 仅 127.0.0.1。

## 9. 目录结构（最终）

```
luci-app-huawei-cpe/
├── Makefile                          # apk 包（前端 + daemon 二进制包声明，CONFIG_USE_APK 下自动出 .apk）
├── README.md
├── LICENSE
├── docs/
│   ├── 01-sdk-analysis.md
│   ├── 02-openwrt-package-standards.md
│   ├── 03-architecture.md            # 本文件
│   └── 04-mvp-plan.md
│
├── root/
│   ├── etc/
│   │   ├── config/huawei_cpe         # UCI 默认配置（装包即带）
│   │   └── init.d/huawei-cpe         # procd init
│   └── usr/sbin/huawei-cpe           # Go 二进制（由 huawei-cpe 包安装）
│
├── src/                              # Go daemon（module: huawei-cpe 或子module）
│   ├── cmd/huawei-cpe/main.go        # run / reload 入口
│   ├── internal/
│   │   ├── config/                   # UCI 读取/校验/热重载  (uci 解析用简单解析器或 uci 二进制方式)
│   │   ├── device/                   # DeviceManager, Connection 租赁, 重连, 身份
│   │   ├── poller/                   # 轮询采集循环, 差分速率, 能力探测
│   │   ├── cache/                    # 状态快照
│   │   ├── db/                       # SQLite 打开/迁移/保留清理
│   │   ├── signalhist/ traffichist/  # 历史写入+聚合查询
│   │   ├── sms/                      # 同步/去重/已读/删除/发送
│   │   ├── eventbus/  events/        # 事件
│   │   ├── notifymgr/ notify/        # 规则+渠道 (tg/smtp/webhook)
│   │   ├── netmon/                   # CPE/Internet 健康状态机
│   │   ├── recovery/                 # 自动恢复
│   │   ├── discover/                 # 设备发现
│   │   └── httpapi/                  # REST(127.0.0.1) + 脱敏
│   └── go.mod / go.sum
│
├── luci/
│   ├── controller/huawei_cpe.lua     # entry: Services → Huawei CPE
│   ├── model/cbi/huawei_cpe/         # Settings/TG/SMTP/Webhook/Rules（带密码框）
│   └── view/huawei_cpe/              # dashboard.htm / sms.htm / signal.htm / traffic.htm / notifications.htm
│       └── (... js 用 luci 自带组件, 无框架)
│
└── po/                               # 可选，MVP 阶段 English 为主，zh-cn 跟进
```

## 10. 依赖

| 依赖 | 版本 | 用途 | 理由 |
|---|---|---|---|
| `github.com/lvcdy/huawei-lte-api-go` | master/最新 tag | CPE 通信 | 硬性要求 |
| `modernc.org/sqlite` | latest | SQLite | 纯 Go 无 CGO，交叉编译友好 |
| 其余 | stdlib | http/server, slog, sql | 避免臃肿 |

> 注意：SDK `go.mod` 要求 `go 1.27`。若 OpenWrt 官方 Go 工具链版本不足，构建期可用较新的 Go 手动交叉编译（`GOOS=linux GOARCH=arm64` 或 mips…），Makefile 提供 fallback。见 docs/04。

## 11. 多设备（架构不写死单设备）

- UCI 允许多个 `config cpe` section；每设备一个实例（连接/轮询/短信/健康彼此独立）。
- 所有 API 端点带 `{id}`；LuCI 设备选择器切换。
- v0.1 默认只配 `main`，但代码路径天然支持 N 台（内存与 SQLite 均按 device_id 隔离）。