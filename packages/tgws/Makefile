# SPDX-License-Identifier: MIT
include $(TOPDIR)/rules.mk

PKG_NAME:=tgws
PKG_VERSION:=0.1.2
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_MAINTAINER:=tgws-openwrt contributors

include $(INCLUDE_DIR)/package.mk

# scripts/build.sh builds static binaries from this checkout before the SDK
# packages them. Release CI downloads only artifacts of the same workflow run.
TGWS_PREBUILT_DIR?=$(TOPDIR)/feeds/tgws/dist
TGWS_BINARY=$(if $(filter aarch64,$(ARCH)),aarch64,$(if $(filter x86_64,$(ARCH)),x86_64,$(if $(filter arm,$(ARCH)),arm,$(if $(filter mipsel,$(ARCH)),mipsel,$(if $(filter mips,$(ARCH)),mips)))))

define Package/tgws
  SECTION:=net
  CATEGORY:=Network
  TITLE:=Transparent Telegram WebSocket proxy
  URL:=https://github.com/trikesh32/tgws-openwrt
  DEPENDS:=+ca-bundle
endef

define Package/tgws/description
  Static tgws executable. Install luci-app-tgws for the OpenWrt service,
  transparent firewall rules, configuration and web interface.
endef

define Build/Prepare
	@test -n "$(TGWS_BINARY)" || { echo "Unsupported architecture: $(ARCH)"; exit 1; }
	@test -s "$(TGWS_PREBUILT_DIR)/tgws-linux-$(TGWS_BINARY)" || { echo "Run TGWS_VERSION=$(PKG_VERSION) sh scripts/build.sh dist first"; exit 1; }
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) $(TGWS_PREBUILT_DIR)/tgws-linux-$(TGWS_BINARY) $(PKG_BUILD_DIR)/tgws
endef

define Build/Configure
endef

define Build/Compile
endef

define Package/tgws/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/tgws $(1)/usr/bin/tgws
endef

# A binary-only upgrade must also replace the running process.
define Package/tgws/postinst
#!/bin/sh
[ -n "$$IPKG_INSTROOT" ] && exit 0
if [ -x /etc/init.d/tgws ] && /etc/init.d/tgws running >/dev/null 2>&1; then
	/etc/init.d/tgws restart
fi
exit 0
endef

$(eval $(call BuildPackage,tgws))
