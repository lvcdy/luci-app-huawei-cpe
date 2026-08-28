#
# Copyright (C) 2026 luci-app-huawei-cpe authors
# This is free software, licensed under the MIT License.
#

include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-huawei-cpe
PKG_VERSION:=0.1.0
PKG_RELEASE:=1

PKG_MAINTAINER:=Huawei CPE Manager authors <maintainers@example.invalid>
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE

# Go daemon 在 src/ 独立子模块编译，需要 golang 工具链
include $(TOPDIR)/../toolchain/golang/golang-package.mk

include $(INCLUDE_DIR)/package.mk

# ----------------------------------------------------------------------
# 包 1：huawei-cpe（Go daemon 二进制）
# ----------------------------------------------------------------------
define Package/huawei-cpe
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Huawei LTE/5G CPE manager daemon
  URL:=https://github.com/lvcdy/luci-app-huawei-cpe
  DEPENDS:=+libc
endef

define Package/huawei-cpe/description
  Daemon to monitor and manage Huawei LTE/5G CPE devices on the LAN
  (e.g. H168-383 / H151-383) using the huawei-lte-api-go SDK.
  Exposes a loopback-only JSON API for the LuCI frontend.
endef

GO_PKG:=./src
GO_PKG_BUILD_PKG:=./cmd/huawei-cpe
GO_PKG_INSTALL_PREFIX?=/usr

define Package/huawei-cpe/install
	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_DIR)/cmd/huawei-cpe/huawei-cpe $(1)/usr/sbin/
endef

# ----------------------------------------------------------------------
# 包 2：luci-app-huawei-cpe（LuCI 前端 + UCI 配置 + init）
# ----------------------------------------------------------------------
define Package/luci-app-huawei-cpe
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=LuCI support for Huawei CPE manager
  URL:=https://github.com/lvcdy/luci-app-huawei-cpe
  DEPENDS:=+luci-base +huawei-cpe +jq
endef

define Package/luci-app-huawei-cpe/description
  LuCI frontend for the Huawei LTE/5G CPE manager daemon.
  Provides Dashboard, SMS, Signal & Traffic pages plus Settings.
endef

define Package/luci-app-huawei-cpe/install
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/controller
	$(INSTALL_DATA) ./luci/controller/huawei_cpe.lua $(1)/usr/lib/lua/luci/controller/

	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/model/cbi/huawei_cpe
	$(INSTALL_DATA) ./luci/model/cbi/huawei_cpe/*.lua $(1)/usr/lib/lua/luci/model/cbi/huawei_cpe/

	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/view/huawei_cpe
	$(INSTALL_DATA) ./luci/view/huawei_cpe/*.htm $(1)/usr/lib/lua/luci/view/huawei_cpe/

	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_DATA) ./root/etc/config/huawei_cpe $(1)/etc/config/

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./root/etc/init.d/huawei-cpe $(1)/etc/init.d/
endef

$(eval $(call BuildPackage,huawei-cpe))
$(eval $(call BuildPackage,luci-app-huawei-cpe))