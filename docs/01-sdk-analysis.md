# 01 · huawei-lte-api-go 实际 API 分析

> 分析日期：2026-08-29，基于 `github.com/lvcdy/huawei-lte-api-go` master 分支源码。

## 1. 模块与版本

- **模块路径**：`github.com/lvcdy/huawei-lte-api-go`（仓库根即模块根）
- **`go.mod`**：`go 1.27`（较新 —— 构建时 OpenWrt 工具链需满足）
- **依赖**：纯标准库，零第三方依赖；自动挂载 CookieJar；内置 CSRF token 刷新与重试
- **风格**：完整移植 Python 版 `Salamek/huawei-lte-api` v2.0.1 的行为与错误模型

## 2. 关键公开 API（与本项目相关的部分）

### 2.1 连接与登录（`session` 包）

```go
// URL 可内嵌凭据：http://admin:pass@192.168.8.1/
// 任一凭据存在时，NewConnection 会自动创建 UserSession 并强制登录
func NewConnection(rawURL string, username, password string,
    timeout time.Duration, requestsClient *http.Client) (*Connection, error)

client := huaweilteapi.NewClient(conn)   // 聚合全部 API 分组

// 会话刷新（CSRF token 重新初始化；登录过期需另行重新登录）
func (s *Session) Reload() error
func (c *Connection) Close()
```

- `client.User`（`*session.User`）：`Login(username, password, forceNewLogin)`、`Logout()`、`StateLogin()`
- 登录错误码已类型化：`LoginInvalidCredentialsError` / `LoginPasswordWrongError` 等（见 §4）

### 2.2 设备（`api/Device.go`）

```go
func (d *Device) Information() (map[string]interface{}, error)      // device/information
func (d *Device) BasicInformation() (map[string]interface{}, error) // device/basic_information
func (d *Device) Signal() (map[string]interface{}, error)           // device/signal
func (d *Device) BootTime() (map[string]interface{}, error)         // device/boot_time
func (d *Device) SetControl(control enums.ControlMode) (interface{}, error) // 1=重启 2=恢复出厂 4=关机
```

`Information()` 返回字段（实测 B310s-22 示例）：`DeviceName`、`SerialNumber`、`Imei`、`Imsi`、`Iccid`、`Msisdn`、`HardwareVersion`、`SoftwareVersion`、`WebUIVersion`、`MacAddress1`、`MacAddress2`、`ProductFamily`、`Classify`、`workmode` —— 满足设备模型与自动识别的需要。

### 2.3 监控（`api/Monitoring.go`）—— Dashboard / 流量核心

```go
func (m *Monitoring) Status() (map[string]interface{}, error)            // monitoring/status
func (m *Monitoring) TrafficStatistics() (map[string]interface{}, error) // monitoring/traffic-statistics
func (m *Monitoring) MonthStatistics() (map[string]interface{}, error)   // monitoring/month_statistics
func (m *Monitoring) StartDate() (map[string]interface{}, error)         // monitoring/start_date
func (m *Monitoring) CheckNotifications() (map[string]interface{}, error)
```

`monitoring/status` 常见字段（不同型号有差异，daemon 需容忍缺失）：
`ConnectionStatus`、`SignalIcon`/`SignalIconNr`、`NetworkType`、`CurrentNetworkType`、`CurrentServiceDomain`、`Roaming`、`OperatorName`、`WanIPAddress`、`Temperature`、`Uptime`、LTE/NR band 相关字段等。

### 2.4 网络（`api/Net.go`）

```go
func (n *Net) CurrentPlmn() (map[string]interface{}, error) // 运营商名（Name 字段）
func (n *Net) NetMode() (map[string]interface{}, error)
func (n *Net) Network() (map[string]interface{}, error)
func (n *Net) Register() (map[string]interface{}, error)
func (n *Net) CellInfo() (map[string]interface{}, error)   // PCI / CellID / EARFCN 等
func (n *Net) Reconnect() (interface{}, error)             // net/reconnect
```

### 2.5 短信（`api/Sms.go`）—— 短信中心核心

```go
type Message struct {
    Index    int               // 设备侧索引 → 去重主键
    Status   enums.SmsStatus   // 0=new 1=read 2=pending 3=send 4=send_failed
    Phone    string
    Content  string
    DateTime time.Time
    Sca      *string
    SaveType enums.SaveMode
    Priority enums.Priority
    Type     enums.SmsType     // 1=单条 2=多部分 5=Unicode ...
    TextMode enums.TextMode
}

func (s *Sms) GetSmsList(page int, boxType enums.BoxType, readCount int,
    sortType enums.SortType, ascending, unreadPreferred bool) (map[string]interface{}, error)
func (s *Sms) GetMessages(page int, boxType enums.BoxType, readCount int,
    sortType enums.SortType, ascending, unreadPreferred bool) ([]Message, error) // 分页迭代读取全部
func (s *Sms) SmsCount() (map[string]interface{}, error)       // 未读/总数
func (s *Sms) SetRead(smsID int) (interface{}, error)          // 按 Index 标记已读
func (s *Sms) DeleteSms(smsID int) (interface{}, error)        // 按 Index 删除
func (s *Sms) SendSms(phoneNumbers []string, message string, smsIndex int,
    sca *string, textMode enums.TextMode, fromDate *time.Time) (interface{}, error)
func (s *Sms) SendStatus() (map[string]interface{}, error)
```

`enums.BoxType`：`BoxTypeLocalInbox=1 / LocalSent=2 / LocalDraft=3 / LocalTrash=4 / SimInbox=5 ...`。

**`GetMessages` 返回 `[]Message`（已解析的结构体），是本项目短信同步的首选接口。**

### 2.6 诊断（`api/Diagnosis.go`）与系统

```go
func (d *Diagnosis) SetDiagnosePing(host string, timeout int) (interface{}, error) // CPE 侧 ping
func (s *System) Deviceinfo() (map[string]interface{}, error)
```

> 注：`DiagnosePing` 是 CPE 侧发起的 ping（用于 CPE 自身诊断），**网络健康监控我们建议在 OpenWrt 侧自己做**（见架构文档），不依赖它。

## 3. 配置分组（`config` 包）

提供 `ConfigGlobal`、`ConfigNetwork`、`ConfigSms`、`ConfigDeviceInformation` 等 20+ 分组，用于读写设备内部配置（非本项目第一版需求，仅列出备查）。

## 4. 错误模型（`session/errors.go`）—— daemon 错误处理依据

- 基类 `ResponseError{Code, Message}`，派生 14 种类型，`errors.As` / `errors.Is` 可穿透嵌套链。
- 便捷判断：`session.IsNotSupported(err)`、`session.IsLoginRequired(err)`、`session.IsSystemBusy(err)`、`session.IsLoginCsrf(err)`、`session.IsWrongSessionToken(err)`。
- 登录错误类型化：`LoginInvalidCredentialsError`（永久性凭据错误，**应停止自动重试**）、`LoginPasswordWrongError`、`LoginAlreadyLoginError` 等。
- `session.Code(err)` 可提取原始错误码（如 `125003` Wrong Session Token、`100002` No Support、`108002` 密码错）。

这是 daemon「CPE 离线 / 认证失败 / API 不支持 / 会话过期」细分处理的关键能力。

## 5. 枚举（`enums` 包）

`ControlMode`（Reboot=1 / Reset=2 / BackupConfig=3 / PowerOff=4）、`NetworkMode`（自动/2G/3G/4G 字符串枚举）、`LTEBand`（`LTEBandB1`、`LTEBandB3`、`LTEBandB7`…位掩码）、`NetworkBand`、`SmsStatus`、`SmsType`、`BoxType`、`TextMode`、`SaveMode`、`SortType`、`ConnectionStatus`（900~906）等。

## 6. 与本项目需求的映射

| 需求 | SDK 支撑 | 备注 |
|---|---|---|
| CPE 登录 / 会话 | `NewConnection` + `User.Login` / `Session.Reload` | 会话过期需重建或重登（daemon 层策略） |
| 设备信息 / 自动识别 | `Device.Information()` | Name/Model/HW/SW/IMEI/IMSI/ICCID/SN 全齐 |
| 网络状态 / Dashboard | `Monitoring.Status` + `Net.CurrentPlmn` + `Net.Network` + `Net.Register` | |
| 信号 | `Device.Signal()` + `Net.CellInfo()` | Band/PCI/CellID/EARFCN 在 CellInfo |
| 流量 | `Monitoring.TrafficStatistics` + `MonthStatistics` | 速率需 daemon 差分计算 |
| 短信（收/读/删/发） | `GetMessages` / `SetRead` / `DeleteSms` / `SendSms` | `Message.Index` 直接作去重键 |
| 重启 CPE | `Device.SetControl(ControlModeReboot)` | |
| 重新连接网络 | `Net.Reconnect()` | 自动恢复可选用 |
| 能力探测 | 各 API 返回 `NotSupportedError` | daemon 探测一次并缓存结果 |

## 7. SDK 缺口清单（供 daemon 层或后续 SDK PR 处理）

### 7.1 daemon 层必须自行解决（不属于 SDK 职责）

1. **会话过期后的重新登录**：SDK 的 `Get/PostSet` 只处理 CSRF 过期（`LoginCsrf → Reload → retry`），不处理**登录态过期**（`LoginRequired`）。daemon 需:检测 `IsLoginRequired` → 调用 `User.Login` 或重建 `Connection`，并配退避。
2. **凭据错误终止策略**：`NewConnection` 登录失败即返回错误；`LoginInvalidCredentialsError` 是永久错误。daemon 需区分「临时错误（重试）」与「永久错误（停止并告警）」。
3. **速率计算**（↓/↑ Mbps）：两个采样点 `TrafficStatistics` 之差 / 时间。SDK 不给，daemon 自算。
4. **能力探测与缓存**：哪些 API 受支持（SMS/Signal/Traffic/Reboot/CellInfo）→ daemon 探测一次并缓存，LuCI 据此动态显示。
5. **网络健康检测**：SDK 只有 CPE 侧 ping。OpenWrt 侧直接探测外网连通（见架构文档），不依赖 SDK。
6. **信号/流量历史、短信库、去重、通知**：全部是 daemon 层责任。

### 7.2 建议后续回馈 SDK 的改进（MVP 不阻塞，暂不改 SDK）

1. **`go.mod` 要求 `go 1.27`**：OpenWrt 稳定分支的 Go 工具链通常落后 1~2 个大版本。建议 SDK 后续评估放宽 go 版本要求（例如 `go 1.22`），便于 OpenWrt `golang-package.mk` 直接构建；MVP 构建期用最新 Go 工具链绕过（见 03-mvp-plan.md）。
2. **方法不接收 `context.Context`**：超时依赖 `http.Client.Timeout`。建议后续增加 ctx 变体（或 daemon 短超时 + 外层保护）。
3. **`monitoring/status` 等返回 `map[string]interface{}`**：字段随机型漂移，建议 SDK 后续提供「常见字段提取器」或强类型结构 + 未知字段保留。
4. **缺少「设备类型/产品族 → 能力矩阵」辅助**：建议 SDK 后续暴露统一的 capability hints。
5. **无 WebSocket/push**：Python 版有事件推送；Go 版暂无。MVP 用轮询，未来可考虑 SDK 侧补 push 后再切换（目前不阻塞）。

## 8. 结论

- **MVP 不需要修改 SDK**。全部需求可用现有公开 API 实现，缺口集中在 daemon 层策略。
- 本项目将 SDK 视为纯依赖：`go get github.com/lvcdy/huawei-lte-api-go`，只调用其公开接口。
- LGPL-3.0 许可：本项目在自己仓库正确声明 attribution（README 注明 API 实现来源与 License 要求）。