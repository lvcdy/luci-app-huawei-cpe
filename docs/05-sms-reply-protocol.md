# 05 · 短信双向收发协议设计（转发 + 回复 + 命令）

> 状态：**设计草案（待用户审阅）** — 仅文档，不实施、不写代码。
> 适用：Phase 4 通知（P4.3 Telegram / P4.4 Email / P4.5 Webhook）实现时配合
> P3.3 已读/删除 API、P3.5 发送 API 一起落地。
>
> 目标：把「转发短信」升级为「**可回复的双向通道**」— 收到短信被推送出去后，
> 用户在 Telegram / Email / Webhook 里即可**直接回复**或发**命令**控制短信。

---

## 1. 设计原则

1. **上下文链优先**：回复一条被转发的短信 = 回复给该短信的**发件人号码**。
   最自然的方式：Telegram 用 **Reply**、Email 用 **Re: 主题**、Webhook 用 **thread_id**。
2. **命令可发现**：所有通道提供一致的命令集（list / send / read / delete / info）。
3. **复用已有 API**：命令 & 回复**只调 daemon 内部服务层**（`smsmgr` 的
   `SendSms / SetRead / DeleteSms`），不直连 CPE；与 P3.3/P3.5 API 同源。
4. **安全默认关**：短信正文默认不转发（`sms_forward_secret=0`）；即使开启转发，
   发送 / 删除等**写操作**需要**白名单 + 二次确认**（可选 PIN）。
5. **不泄密**：正文绝不出现在日志 / 错误链 / 无权限返回里；写操作成功与否只回
   「已发送」「发送失败」等脱敏结果。

---

## 2. 统一抽象：通道命令与会话

```go
// 所有支持回复的通道实现同一接口（Phase 4）。
type Channel interface {
    Name() string                       // "telegram" | "email" | "webhook"
    // 转发短信时返回上下文句柄，供后续回复定位
    HandleForward(ctx, sms sms.Record) (threadID string, err error)
    // 处理回复/命令（用户 → 短信）
    HandleInbound(ctx, msg InboundMsg) error
}

// InboundMsg：任一通道进来的用户消息，归一化表示。
type InboundMsg struct {
    Channel   string // 来源通道
    SenderID  string // 通道侧发送者（tg chat_id / 邮件地址 / webhook key）
    ThreadID  string // 若为「回复」，指向被转发短信的 threadID；否则空
    Command   string // 命令字（list/send/read/delete/info/help）
    Args      []string
    FreeText  string // send 命令的正文 / 无命令时的普通回复
}
```

**定位被回复短信**：`threadID` 内部编码为 `{deviceID}|{localSmsID}`（本地 SQLite id，
不是 CPE index——外部不可见 CPE index，避免泄露且防枚举）。

---

## 3. Telegram Bot（P4.3 扩展）

### 3.1 转发格式（含上下文）

```
📩 新短信 · Huawei CPE (main)
发件人: +86 138 0013 8000
时间:   2026-08-29 09:48

短信正文（如果 sms_forward_secret=1 才有，否则显示 [正文未启用转发]）

— 回复本条消息即可回复该发件人 —
命令: /sms list | /sms send | /sms read | /sms delete | /sms info
```

- 转发时用 `reply_markup` 空（不占位）；消息本身即上下文锚点。
- 若 `sms_forward_secret=0`：不显示正文，但**仍显示发件人 + 时间 + 命令提示**，
  用户可 `/sms read <id>` 后收到正文（read 命令本身受白名单 +（可选）PIN 控制）。

### 3.2 回复 → 发短信

- 用户对转发消息 **Reply**，正文即短信内容，直接发送给该短信发件人。
- 内置确认：`你要回复 +8613800138000：……对吗？` + 内联按钮
  `✅ 发送 | 取消`（Telegram inline keyboard，`callback_data` 带一次性 ticket）。
- ticket 生命周期 5 分钟、单次有效、存储于内存；不出现明文号码之外的信息。

### 3.3 命令

| 命令 | 说明 | 输出示例 |
|---|---|---|
| `/sms help` | 帮助 | 命令清单 + 使用说明 |
| `/sms list` | 最近 10 条（全部；`list unread` 只未读） | `#12  10086  …` |
| `/sms info <id>` | 单条详情（正文仅在有权时返回） | 发件人/时间/状态/正文 |
| `/sms send <phone> <text>` | 发送（需确认） | `已发送` / `发送失败（脱敏）` |
| `/sms read <id>` | 标记已读（复用 P3.3 SetRead） | `#12 已读` |
| `/sms delete <id>` | 删除（需确认；不可逆） | `#12 已删除` |

- 权限：`chat_id ∈ allowed_chat_ids` 才响应；发送/删除额外要求 **二次确认**。
- 可选 PIN：`option pin '0'` 或配置 6 位 PIN 时，写命令需先 `/sms auth <pin>`。

### 3.4 消息超过 Telegram 限制

- 正文 > 3500 字符：截断 + `…（全文请用 /sms info <id>）`。

---

## 4. Email（P4.4 扩展）

### 4.1 转发邮件格式

```
To:      用户
From:    [Huawei CPE] <from@...>
Subject: Re: [Huawei CPE: 12] 来自 +8613800138000     ← 携带本地短信 id
日期:    2026-08-29 09:48

短信正文（sms_forward_secret=1 时）
```

### 4.2 回复规则（主题行约定）

- 用户直接 **Reply** 该邮件 → daemon 解析主题中 `[Huawei CPE: <id>]`：
  - 找到 id → 把**邮件正文**作为短信内容发送给该短信发件人；
  - 找不到/已删 → 回一封 `未找到短信 #id`（不带原正文）。
- 主题可改：`Re: [Huawei CPE: 12] 任意标题` 依然按 id=12 处理。
- **命令邮件**：主题 `[Huawei CPE: cmd] list` / `send +86138 你好`（正文），
  解析约定见 5.2 统一命令语法；回执邮件格式与转发邮件一致。

### 4.3 安全

- 仅接受来自 `option to` 白名单（或配置的 reply_allow_from）的发件人；
- 主题/正文解析失败 → 忽略并记（脱敏）日志；绝不把号码写进日志。

---

## 5. Webhook（P4.5 扩展）

### 5.1 转发（出站）

```json
{
  "event": "sms.received",
  "device_id": "main",
  "sms_id": 12,
  "from": "+8613800138000",
  "at": "2026-08-29T09:48:00+08:00",
  "content": "…" ,
  "reply": {
    "url": "http://127.0.0.1:9090/api/v1/out/sms/reply/12",
    "method": "POST"
  }
}
```

- `sms_forward_secret=0` 时 `content` 字段省略。
- 出站 `reply.url` 是 daemon 内部回拨地址（仅 127.0.0.1，外部不可达），
  供接收方（用户自己的机器人服务）回调回复。

### 5.2 入站（回复/命令，统一语法）

接收方任意实现，向 daemon 发 **POST**（可选 `X-Api-Key`）：

```json
// 回复被转发的短信（用转发出带的 sms_id）
{ "reply_to": 12, "channel": "telegram-custom", "sender": "mysvc", "text": "收到，稍等" }

// 命令（等价于 Telegram 命令）
{ "command": "send", "args": ["+8613800138000", "你好"], "channel": "mysvc", "sender": "mysvc" }
{ "command": "read",  "args": ["12"], ... }
{ "command": "delete","args": ["12"], ... }
```

- 权限：`webhook` section 增加 `option api_key ''`（可选）；sender 未认证则忽略。
- 写操作同样回执确认（响应 JSON `{"ok": true, "result": "已发送"}`）。

---

## 6. 统一命令解析与错误

### 6.1 统一命令语法（所有通道一致）

```
list [unread]          # 最近10条；unread 只未读
info <id>
send <phone> <text>    # 需要确认
read <id>
delete <id>            # 需要确认
help
```

### 6.2 错误全部脱敏

| 场景 | 返回 |
|---|---|
| 号码不在白名单 | 忽略（连「无权限」都不回，防枚举） |
| 短信不存在/已删 | `未找到短信 #id` |
| CPE 发送失败 | `发送失败（请检查 CPE 或稍后重试）` |
| 未开启转发 | 通道照常可用命令，转发提示「未开启 forwarding」 |

---

## 7. 安全与配置（新增 UCI 段，均为 Phase 4 实现）

```
config notify 'telegram'
    option enabled '1'
    option bot_token ''
    option allowed_chat_ids ''     # 已有
    option pin '0'                 # 0=不启用；否则写操作需先 /sms auth <pin>

config notify 'smtp'
    ...
    option reply_from_allow ''     # 允许回复来源（默认 = option to 白名单）

config notify 'webhook'
    option url ''                  # 已有（出站）
    option api_key ''              # 入站可选

config rules 'sms_received'
    option sms_forward_secret '0'  # 已有：0=正文不外发
```

- **写操作一律**：白名单(通道) → (可选 PIN) → 二次确认 ticket（5 分钟单次）。
- **删除不可逆**：`delete` 确认文案明确「不可恢复」。
- **正文不外发**：`sms_forward_secret=0` 时正文不出 daemon（本地 SQLite 仍存）。
- 所有命令处理与转发都走 `smsmgr`，不经日志泄露正文/号码。

---

## 8. 落地范围与顺序（审阅通过后）

| 阶段 | 内容 | 依赖 |
|---|---|---|
| A | `smsmgr.SendSms`（P3.5）+ 内部服务接口抽取 | 现有 sms 包 |
| B | `notify/tg` 回复 + 命令 + 确认 ticket（P4.3 扩展） | A |
| C | `notify/smtp` 主题解析回复 + 命令邮件（P4.4 扩展） | A |
| D | `notify/webhook` 出站 envelope + 入站 POST（P4.5 扩展） | A |
| E | LuCI 设置页（pin / reply_allow / api_key）与 UI | pen.dev 审阅后 |

> 每阶段一个可编译提交；回复链路单测：mock Telegram API / 假 SMTP / httptest。