# luci-app-huawei-cpe

OpenWrt LuCI 应用 —— 局域网内 Huawei LTE/5G CPE（如 H168-383 / H151-383）管理。

> 注意：本软件项目自身的许可为 MIT，但运行时链接的 [huawei-lte-api-go](https://github.com/lvcdy/huawei-lte-api-go)（HiLink 通信 SDK）为 **LGPL-3.0**。请遵守其许可条款（如动态链接、提供对应源码等）。

## 功能

- 通过 HiLink API 管理一台或多台 Huawei CPE（信息、信号、流量统计、短信、网络连接等）
- LuCI 图形界面（配置、状态监控、短信、重启/复位）
- 故障检测与自动恢复（ping 探测、自动重启）
- 短信/断线通知（Telegram / SMTP / Webhook）
- 仅监听回环地址 `127.0.0.1` 的本地控制 API，凭据/短信内容不进入日志与 API 返回

## 构建与运行

软件包通过 OpenWrt apk 打包（`CONFIG_USE_APK` 下自动生成 `.apk` 格式），Makefile 同时声明
`huawei-cpe`（Go daemon）与 `luci-app-huawei-cpe`（前端）两个包。

见 `docs/` 下的设计与实现文档。