# 02 · OpenWrt / LuCI 软件包结构调研

> 依据当前 OpenWrt 官方 feed 规范（packages feed 与 luci feed）。
> **包管理器：apk**（OpenWrt 24.10+ / 25.x 已由 opkg 迁移至 apk）。

## 1. 软件包布局（apk / feeds 标准）

OpenWrt 软件包由 `Makefile` 声明、构建系统统一产出 `.apk`（`CONFIG_USE_APK` 下自动切到 apk 格式，无需额外改动包声明）：

```
luci-app-huawei-cpe/
├── Makefile          # 包构建入口（PKG_NAME=luci-app-huawei-cpe）
├── root/             # 覆写到目标根目录，等价于安装树的 $(1)
│   ├── etc/init.d/huawei-cpe
│   └── usr/sbin/huawei-cpe       # Go 编译产物安装位置（或 /usr/bin）
├── src/              # Go daemon 源码（可选，也可放 ./go/）
├── luci/             # LuCI 前端（镜像到 /usr/lib/lua/luci/... 或 /www/...）
│   ├── controller/
│   ├── model/
│   ├── view/
│   └── i18n/
├── po/               # 翻译（可选，第一版可跳过）
└── docs/
```

### Makefile 关键点（apk 时代）

```makefile
include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-huawei-cpe
PKG_VERSION:=0.1.0
PKG_RELEASE:=1

# 若需 Go 编译：
# include $(TOPDIR)/../toolchain/golang/golang-package.mk

include $(INCLUDE_DIR)/package.mk
```

- 同一份 Makefile 在 `CONFIG_USE_APK` 下自动产出 `<name>-<version>.apk`，**依赖语法不变**（`+pkg` 前缀表示依赖）；
- 用 `Package/…/install` 把 `root/` 下文件 `$(INSTALL_DIR)` + `$(CP)` 进 `$(1)`；
- Go daemon 通常由独立的二进制包 `huawei-cpe`（纯 Go Package）负责编译安装，luci-app 的 Makefile 声明 `DEPENDS:=+huawei-cpe`，前端单独打包 → 用户可按需只装 daemon。
- `procd` init script 放 `root/etc/init.d/`；安装时 `default_postinst` 会自动 enable（`/etc/init.d/* enable`），无需手写 enable 脚本。
- apk 的 conffile 保留通过 `lib/apk/packages/*.conffiles` 实现（`/etc/config/*` 自动被视为 conffile）；升级保留用户配置。
- 脚本映射：`preinst→pre-install`、`postinst→post-install`、`postrm→post-deinstall`（均由构建系统自动注入 `default_postinst`/`default_prerm`）。

## 2. procd init（`/etc/init.d/huawei-cpe`）

OpenWrt 官方建议，daemon 由 procd 托管（自动拉起、崩溃重启、热 reload）：

```sh
#!/bin/sh /etc/rc.common
USE_PROCD=1
START=95
STOP=10

START_SERVICE_CMD="start_service_run"
start_service() {
    procd_open_instance
    procd_set_param command /usr/sbin/huawei-cpe run
    procd_set_param respawn 3600 5 5        # 崩溃 5 秒内拉起，1 小时内最多 5 次
    procd_set_param file /etc/config/huawei_cpe   # 配置文件变化触发 reload
    procd_set_param term_timeout 5
    procd_close_instance
}
reload_service() {
    procd_send_signal huawei-cpe       # SIGUSR1 → daemon 重载配置
}
stop_service() {
    procd_kill huawei-cpe
}
```

- `procd_set_param file`：`/etc/config/huawei_cpe` 变更自动触发 reload；
- crash → `respawn` 自动拉起；
- 日志：daemon 直接 `log`/`logger` 到 syslog，用户 `logread` 查看；
- daemon 不做 daemonize（procd 已托管）。

## 3. UCI 配置（`/etc/config/huawei_cpe`）

UCI 文件由 `uci` 工具读写，无需 schema；默认值与校验在 LuCI model 层或 daemon 启动时处理。多设备用 section 列表：

```
config cpe 'main'
    option enabled '1'
    option name 'H168-383 主路由'
    option host '192.168.8.1'
    option username 'admin'
    option password '...'
    option polling_interval '60'

config notification 'telegram' ...
config smtp 'smtp' ...
```

UCI 不支持内层嵌套数组 → 白名单、规则等用「逗号分隔字符串」或独立 section + 列表：
- 逗号分隔：`option allowed_chat_ids '111,222'`
- 或 `list allowed_chat_id '111'`（`uci` 的 list 语义）。

## 4. LuCI 前端约定（luci-base / 当前 24.10+ 版本）

- **Controller**：`luci/controller/huawei_cpe.lua`，`function index()` 内 `entry()` → 位于 `Services` 菜单下：
  ```lua
  entry({"admin", "services", "huawei_cpe"}, cbi("huawei_cpe/settings", ...), _("Huawei CPE"), 60)
  ```
  需要 `.user` 权限检查（`require "luci.dispatcher"` 已内建 error）。
- **Model（CBI）**：用于 Settings 页，`luci/model/cbi/huawei_cpe/*.lua`，`uci:map("huawei_cpe", ...)`。
- **View（模板）**：只读/动态页（Dashboard、SMS、Signal、Traffic、Notifications）用 `luci/view/...` + `client-side JS`，通过 **RPC** 到 daemon HTTP API（见架构文档 §API）。
- **前端不引大型框架**：官方立场 —— 用 LuCI 自带的 `luci.dom` / `E()` 组件、少量 event 总线（`poll.js` 已由 luci-base 提供，支持 `handlePageLoad` + 间隔轮询）。
- 状态动态获取：首个页面用 `XHR` 拉 `http://127.0.0.1:PORT/api/...`（daemon 内部 HTTP），之后靠 luci-base 的 `poll` 机制定时刷新。
- **翻译**：`po/` 目录（`po/templates/zh-cn/huawei_cpe.pot` 等），可选。

### 菜单结构（第二版再拆独立页，第一版单页即可）

```
Services
└── Huawei CPE
    ├── Dashboard      ← 单页包含：状态/信号/流量概览
    ├── SMS
    ├── Signal         （趋势图）
    ├── Traffic
    ├── Notifications
    └── Settings
```

## 5. 构建（OpenWrt SDK / 本仓库直接构建）

- 完整 OpenWrt 源码树：`./scripts/feeds update -a; ./scripts/feeds install luci-app-huawei-cpe`，再 `make package/…`。
- 单包调试：把仓库软链到 `feeds/luci/applications/` 或用 `-d` 直接 feed 引用。
- Go 子包用 `golang-package.mk`（`GO_PKG:=…`），交叉编译自动处理。
- 本仓库自包含构建（源码树之外）说明见 03-mvp-plan.md §构建。

## 6. 安全与资源约定

- daemon HTTP API **只监听 127.0.0.1**；LuCI 与 daemon 同机，无需跨网。
- LuCI 页面加载 daemon 数据走本机回环，不暴露 WAN。
- 密码字段：UCI 明文存储在 `/etc/config/huawei_cpe`（OpenWrt 惯例），但 LuCI CBI 用 `password` 类型输入框（不回显），daemon 不输出密码到日志/API。
- 运行时数据放 `/var/run/huawei-cpe/`（内存）与 `/var/lib/huawei-cpe/`（SQLite 持久，tmpfs 问题见 03 §存储策略）。