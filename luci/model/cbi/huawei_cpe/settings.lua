-- Huawei CPE Manager — Settings (CBI)
--
-- 管理 UCI 配置（/etc/config/huawei_cpe）中的主设备 section。
-- 安全约束：
--   - 密码字段不回显（cfgvalue 返回空），留空提交时保留原值
--   - 保存后显式触发 `service huawei-cpe reload`（SIGUSR1 重读配置）
--   - 不在日志/错误信息中输出密码

local sys = require "luci.sys"

local m = Map("huawei_cpe",
	translate("Huawei CPE"),
	translate("Configure the Huawei LTE/5G CPE monitored by the huawei-cpe daemon. " ..
		"After saving, the daemon reloads automatically."))

-- 仅当配置存在时渲染（控制器已做菜单保护，此处兜底）
if not m.uci:get("huawei_cpe", "main") then
	m.uci:set("huawei_cpe", "main", "cpe")
end

local s = m:section(NamedSection, "main", "cpe", translate("Main device"))
s.addremove = false

-- 启用开关
local en = s:option(Flag, "enabled", translate("Enabled"),
	translate("Enable polling and monitoring for this device."))
en.rmempty = false

-- 显示名称
local name = s:option(Value, "name", translate("Name"),
	translate("Display name shown in LuCI pages."))
name.rmempty = false
name.default = "Huawei CPE"

-- CPE 管理地址
local host = s:option(Value, "host", translate("Host address"),
	translate("LAN address of the CPE management interface, e.g. 192.168.8.1."))
host.rmempty = false
host.datatype = "host"

-- 登录用户名
local user = s:option(Value, "username", translate("Username"),
	translate("Web login username of the CPE (usually admin)."))
user.rmempty = false
user.default = "admin"

-- 密码：绝不回显；留空提交时保留当前值
local pw = s:option(Value, "password", translate("Password"),
	translate("Web login password of the CPE. Leave empty to keep the current password. " ..
		"Stored only in UCI config and daemon memory; never logged."))
pw.password = true
pw.rmempty = true
-- 回显为空（不回显真实密码）
function pw.cfgvalue(self, section)
	return ""
end
-- 空值不覆盖
function pw.write(self, section, value)
	if value ~= nil and value ~= "" then
		Value.write(self, section, value)
	end
end

-- 轮询间隔（秒）
local poll = s:option(Value, "polling_interval", translate("Polling interval (s)"),
	translate("Interval in seconds between status polls. Lower values react faster but " ..
		"generate more requests to the CPE."))
poll.datatype = "range(10, 3600)"
poll.rmempty = false
poll.default = "60"

-- 短信同步间隔（秒）——Phase 3 生效，此处先提供配置入口
local smsi = s:option(Value, "sms_sync_interval", translate("SMS sync interval (s)"),
	translate("Interval in seconds between SMS inbox synchronizations."))
smsi.datatype = "range(10, 3600)"
smsi.rmempty = false
smsi.default = "30"

-- 保存提交后：显式通知 daemon 重载（procd 文件监听也会触发，双保险）
function m.on_after_commit(self)
	-- reload 通过 procd_send_signal 发送 SIGUSR1；未运行时静默忽略
	sys.call("/etc/init.d/huawei-cpe reload >/dev/null 2>&1")
end

return m
