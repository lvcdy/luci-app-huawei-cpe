-- Huawei CPE Manager — LuCI 控制器
--
-- 职责：
--   1. 在 Services 菜单下注册 Huawei CPE 入口与子页面（Dashboard/Signal/Traffic/Settings）。
--   2. 提供 /api/ 代理节点：浏览器无法直接访问路由器回环地址 127.0.0.1:9090，
--      因此前端请求先到达 LuCI（Lua），再由 Lua 用 nixio socket 转发到本地
--      Go daemon（仅监听回环），并把 JSON 响应原样透传回浏览器。
--
-- 安全：代理只转发到 127.0.0.1，不对外暴露任何后端地址；代理函数本身受
-- LuCI 登录态（.sysauth）保护，未登录无法调用。

module("luci.controller.huawei_cpe", package.seeall)

local nixio = require "nixio"

-- 与 src/internal/httpapi.DefaultAddr 保持一致。
local DAEMON_HOST = "127.0.0.1"
local DAEMON_PORT = 9090

-- 转发超时（秒）。daemon 读取均为内存缓存/本地 SQLite，正常应远小于此值。
local TIMEOUT = 8

function index()
	-- 配置缺失时不注册菜单（避免在未安装 daemon 的固件上出现死入口）。
	if not nixio.fs.access("/etc/config/huawei_cpe") then
		return
	end

	-- 总入口：重定向到 Dashboard。
	local root = entry({"admin", "services", "huawei_cpe"},
		alias("admin", "services", "huawei_cpe", "dashboard"),
		_("Huawei CPE"), 60)
	root.dependent = true

	-- 子页面。order 值越小越靠前。
	entry({"admin", "services", "huawei_cpe", "dashboard"},
		template("huawei_cpe/dashboard"), _("Overview"), 10)

	entry({"admin", "services", "huawei_cpe", "signal"},
		template("huawei_cpe/signal"), _("Signal"), 20)

	entry({"admin", "services", "huawei_cpe", "traffic"},
		template("huawei_cpe/traffic"), _("Traffic"), 30)

	-- SMS 菜单在 Phase 3 引入；能力降级（无 SMS 设备隐藏）由该阶段处理。

	entry({"admin", "services", "huawei_cpe", "settings"},
		cbi("huawei_cpe/settings"), _("Settings"), 90)

	-- API 代理：.leaf = true 允许其后的任意路径段（/api/v1/...）原样进入处理函数。
	-- call() 目标函数默认受登录态保护。
	local api = entry({"admin", "services", "huawei_cpe", "api"}, call("api_proxy"))
	api.leaf = true
end

-- ---------------------------------------------------------------------------
-- API 代理
-- ---------------------------------------------------------------------------

-- 从当前请求路径中提取 /api/... 子路径（含查询串），作为转发目标。
-- LuCI 会把 /admin/services/huawei_cpe/api/v1/devices 拆解为路径段数组，
-- 这里定位 "api" 段，把其后所有段重新拼回（对应 daemon 的 /api/v1/...）。
local function api_subpath()
	local ctx = require("luci.dispatcher").context
	local req = ctx and ctx.request or {}
	local start
	for i, seg in ipairs(req) do
		if seg == "api" then
			start = i
			break
		end
	end
	if not start then
		return nil
	end

	local parts = {}
	for i = start, #req do
		parts[#parts + 1] = req[i]
	end
	local path = "/" .. table.concat(parts, "/")

	local qs = luci.http.getenv("QUERY_STRING") or ""
	if qs ~= "" then
		path = path .. "?" .. qs
	end
	return path
end

-- 通过 nixio socket 向 daemon 发起一次 HTTP/1.1 请求（Connection: close）。
-- 返回 status_code, body；失败返回 nil, err。
local function daemon_request(method, path, body)
	local sock, err = nixio.connect(DAEMON_HOST, DAEMON_PORT)
	if not sock then
		return nil, "daemon not reachable: " .. tostring(err or "connect failed")
	end

	-- 收发超时，避免 daemon 挂起时 LuCI 请求阻塞。
	pcall(function()
		sock:setopt("socket", "sndtimeo", TIMEOUT)
		sock:setopt("socket", "rcvtimeo", TIMEOUT)
	end)

	body = body or ""
	local req = method .. " " .. path .. " HTTP/1.1\r\n"
		.. "Host: " .. DAEMON_HOST .. ":" .. DAEMON_PORT .. "\r\n"
		.. "Connection: close\r\n"
		.. "Accept: application/json\r\n"
		.. "Content-Type: application/json\r\n"
		.. "Content-Length: " .. #body .. "\r\n"
		.. "\r\n"

	local ok, serr = sock:send(req)
	if not ok then
		sock:close()
		return nil, "send failed: " .. tostring(serr)
	end
	if #body > 0 then
		local ok2, serr2 = sock:send(body)
		if not ok2 then
			sock:close()
			return nil, "send body failed: " .. tostring(serr2)
		end
	end

	-- 因为 Connection: close，读到对端关闭即为完整响应。
	local chunks = {}
	while true do
		local c = sock:recv(65536)
		if not c or #c == 0 then
			break
		end
		chunks[#chunks + 1] = c
	end
	sock:close()

	local raw = table.concat(chunks)
	-- 分离状态行与头体。
	local status = tonumber(raw:match("^HTTP/%d%.%d (%d+)")) or 502
	local sep = raw:find("\r\n\r\n", 1, true)
	local resp_body = sep and raw:sub(sep + 4) or ""
	return status, resp_body
end

-- 通用响应：以 JSON 形式把 daemon 结果（或错误）写回浏览器。
local function respond_json(status, body)
	luci.http.status(status, status == 200 and "OK" or "Error")
	luci.http.prepare_content("application/json")
	luci.http.write(body or "")
end

-- 代理入口：转发任意方法到本地 daemon。
function api_proxy()
	local method = luci.http.getenv("REQUEST_METHOD") or "GET"
	-- 仅放行只读/幂等的常用方法；写操作（短信等）在后续阶段按需放开。
	if method ~= "GET" and method ~= "POST" and method ~= "PUT" and method ~= "DELETE" then
		respond_json(405, '{"error":"method not allowed"}')
		return
	end

	local path = api_subpath()
	if not path then
		respond_json(400, '{"error":"missing api path"}')
		return
	end

	-- 透传请求体（POST/PUT 使用）。
	local body = ""
	if method == "POST" or method == "PUT" then
		body = luci.http.content() or ""
	end

	local status, resp = daemon_request(method, path, body)
	if not status then
		-- daemon 未运行/不可达：返回 503，前端据此显示降级状态。
		respond_json(503, '{"error":"daemon unavailable","detail":' ..
			string.format("%q", tostring(resp)) .. '}')
		return
	end

	respond_json(status, resp)
end
