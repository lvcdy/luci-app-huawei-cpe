# 04 · v0.1 MVP 实施计划

> 遵循用户指定的 Five-Phase 顺序，但 MVP 聚焦到「先跑通、可靠」。
> 每个阶段结束有验收标准；本计划可作为后续 task 拆分与进度追踪的依据。

## 里程碑（M-series）

```
M1 骨架: OpenWrt 包 + procd + UCI + daemon 生命周期 + 日志       (Phase 1 前半)
M2 CPE 通信: 登录/设备信息/状态/信号/网络 → 内存缓存 + HTTP API   (Phase 1 后半)
M3 LuCI: Dashboard / Signal / Traffic 页面                       (Phase 2)
M4 SMS: SQLite + 同步 + 去重 + 列表 UI                           (Phase 3)
M5 通知: Telegram / Email / Webhook + 规则                        (Phase 4)
M6 健康/恢复: Netmon + Recovery(默认关) + Reboot action           (Phase 5)
M7 打磨: 错误处理/退避/测试/文档/打包构建                         (贯穿)
```

## 阶段与任务分解

### Phase 1 — OpenWrt 包 + daemon 骨架 + CPE 接入

| # | 任务 | 产出 | 验收 |
|---|---|---|---|
| P1.1 | 仓库骨架 | `Makefile`、`root/`、`docs/`、`.gitignore` | `make` 在 OpenWrt SDK 下可编译（初期本机直接构建） |
| P1.2 | UCI 默认配置 | `root/etc/config/huawei_cpe` | `uci show huawei_cpe` 输出全部 section |
| P1.3 | procd init | `root/etc/init.d/huawei-cpe` | `service huawei-cpe start/stop/restart/reload` 正常；崩溃自动拉起(respawn) |
| P1.4 | daemon 生命周期 | `cmd/huawei-cpe/main.go` + `internal/config` | run 读 UCI；SIGUSR1 重载；SIGTERM 优雅退出；syslog 有日志 |
| P1.5 | 依赖接入 | `go.mod` 引入 SDK + `modernc.org/sqlite` | `go build` 通过；`go vet` 干净 |
| P1.6 | 设备实例 | `internal/device`：Connection 创建/持有/重连；凭据内存态 | 单元测试：创建/重连/永久错误停止（mock server） |
| P1.7 | 轮询采集 | `internal/poller`：status/signal/information/net/traffic；差分速率；能力探测缓存 | 单元测试：mock 返回固定 XML/JSON 快照→快照字段正确 |
| P1.8 | 缓存 | `internal/cache`：状态快照 5~10s | API 读取内存，不触发 CPE |
| P1.9 | HTTP API（部分） | `internal/httpapi`：`/status` `/devices` `/devices/{id}` | `curl 127.0.0.1:9090/api/v1/devices` 返回脱敏 JSON |
| P1.10 | daemon 测试基建 | mock HTTP handler 模拟 CPE（`internal/testutil`） | `go test ./...` 绿 |

**M2 完成标志**：模拟 CPE（或真机）登录成功→LuCI 能从前端拿到设备信息/信号/网络/流量。

### Phase 2 — LuCI Dashboard / Signal / Traffic

| # | 任务 | 产出 | 验收 |
|---|---|---|---|
| P2.1 | Lua controller | `luci/controller/huawei_cpe.lua` | Services 菜单出现 Huawei CPE |
| P2.2 | Dashboard view | `view/huawei_cpe/dashboard.htm`（状态卡+信号+流量概览，poll 刷新） | 真机/模拟数据正确渲染；设备离线显示降级 |
| P2.3 | Signal 页 | `signal.htm` + 历史聚合 API | rsrp/rsrq/sinr 趋势图（原生 canvas/svg，无框架）；h1/d7/d30 |
| P2.4 | Traffic 页 | `traffic.htm` | 当前/总量/日/月；速率图 |
| P2.5 | Settings（CPE 部分） | CBI `settings.lua`（host/user/password/polling_interval） | 保存→`uci commit`→daemon reload |
| P2.6 | 能力降级 | 页面按 `Caps` 显示/隐藏 | 无 SMS 设备→SMS 菜单隐藏且无 JS 报错 |

**M3 完成标志**：LuCI 全部页面在模拟数据+真机上无报错，刷新走缓存。

### Phase 3 — SMS

| # | 任务 | 产出 | 验收 |
|---|---|---|---|
| P3.1 | DB schema + 迁移 | `internal/db` + `sms` 表 | `sqlite3` 查看表结构正确 |
| P3.2 | SMS 同步循环 | `internal/sms`：`GetMessages`→写库→去重 `UNIQUE(device_id,cpe_index)`→事件 `sms.received` | 单元测试：重复同步不重复入库、不重复发事件 |
| P3.3 | 已读/删除（双向） | API POST read / DELETE → CPE `SetRead/DeleteSms` + 本地 | 模拟 CPE 验证回写 |
| P3.4 | SMS 列表 UI | `sms.htm`：全部/未读/搜索/号码筛选/排序/未读计数 | 模拟数据交互通过 |
| P3.5 | 发送（可选） | `SendSms` 接入 | 真机可选 |

**M4 完成标志**：短信自动入库；重复同步无重复通知；未读计数正确。

### Phase 4 — 通知

| # | 任务 | 产出 | 验收 |
|---|---|---|---|
| P4.1 | EventBus | `internal/eventbus` | 订阅/发布/缓冲/失败不阻塞单测 |
| P4.2 | 通知规则 | `notifymgr`：事件→过滤→渠道启用 | 规则表生效单测 |
| P4.3 | Telegram | `/status /sms /help`；ChatID 白名单；token 安全 | mock Telegram API 单测；权限隔离验证 |
| P4.4 | Email SMTP | smtp（SSL/STARTTLS），主题 `[Huawei CPE] ...` | mock SMTP 单测 |
| P4.5 | Webhook | POST JSON，限时，失败仅日志 | 单元测试（httptest server） |
| P4.6 | Notifications UI + Settings | CBI 表单（tg/smtp/webhook/rules）+ events 页 | 保存生效；token 不回显 |
| P4.7 | 短信过滤 | all/whitelist/keyword；`sms_forward_secret` 默认关 | 单测：白名单外短信不转发 |

**M5 完成标志**：模拟出 `sms.received` / `cpe.offline` 事件 → Telegram/Email/Webhook 收到且内容正确；token 不出现在日志。

### Phase 5 — 健康监控 + 自动恢复

| # | 任务 | 产出 | 验收 |
|---|---|---|---|
| P5.1 | Netmon | CPE 可达(ping/API) + Internet 连通(ICMP/TCP 223.5.5.5:53) 状态机 | 单测：离线/恢复状态转换 |
| P5.2 | 自动恢复 | `recovery`：等待→重测→`Net.Reconnect`→(可选)Reboot；max_attempts+cooldown | 单测：防抖、冷却、最大次数 |
| P5.3 | Reboot action | API `POST …/reboot` (202) | 模拟 CPE 收到 reboot |

**M6 完成标志**：CPE 掉线 → LuCI 显示 `CPE: Offline`；Internet 断 → `CPE: Online / Internet: Offline`；自动恢复默认关闭且不误触发。

### Phase 6（贯穿）— 正确性、安全、文档、打包

| # | 任务 |
|---|---|
| P6.1 | 错误/退避/重试统一策略review；全部秘密脱敏核查（日志/API/错误链） |
| P6.2 | 单元测试补齐：CPE offline、auth failure、API timeout、SMS parsing、duplicate SMS、notification failure |
| P6.3 | OpenWrt 打包验证（SDK 或手动交叉编译）；README（含 SDK attribution） |
| P6.4 | 真实设备验证记录（H168-383 / H151-383）：日志脱敏、能力矩阵实际值 |

## 风险与对策

| 风险 | 对策 |
|---|---|
| SDK `go 1.27` vs OpenWrt 工具链旧 | 本机/CI 用新 Go 交叉编译兜底；SDK 提 PR 降版本（不阻塞 MVP） |
| SQLite 纯 Go 驱动在 mips/arm 表现 | `modernc.org/sqlite` 支持多架构；若体积/性能问题→降级开关或用 `mattn`（需 CGO） |
| 不同型号字段差异大 | 能力探测 + 字段容忍解析（缺省显示 Unavailable/隐藏），全部走 SDK map，不写死型号 |
| `monitoring/status` 分片短信 60s 窗口 | 沿用 SDK `GetMessages` 语义，追加同步节流 |
| Telegram 消息超过长度 | 文本模板截断；`/sms` 分页 |
| 重启 CPE 后 daemon 会话全失效 | DeviceManager 统一重连 + 事件; 不 panic |
| OpenWrt tmpfs 掉电丢 SQLite | 持久化在 `/var/lib/huawei-cpe`（overlay 挂载区域），启动时校验完整性 |
| LuCI 版本差异 | 只用 luci-base 稳定 API；页面 JS 用原生 + `luci.dom`，无框架 |

## 明确不做（v0.1 范围外）

- 高级过滤规则引擎（sender regex 等）→ 未来版本
- SMA push / WebSocket 实时推送
- 多运营商逻辑、SMS 模板、WLAN 管理、USSD、PIN 管理
- 跨平台（Windows/macOS 客户端）、多语言完整翻译
- 修改 SDK 本身（除非发现必须缺口，先评估再动手）

## 开始执行顺序（下一步）

1. **P1.1–P1.4**（骨架+procd+UCI+生命周期）—— 先让包能装能启停。
2. **P1.5–P1.9 + mock 基建** —— 打通 daemon 采集与 API。
3. **P2 LuCI 页面** —— 可见成果。
4. 之后按 P3→P4→P5→P6。

> 每步 PR 粒度：一个里程碑一个可编译提交；`go test` 与 `go vet` 在 CI 或本机绿。