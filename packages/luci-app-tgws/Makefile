# SPDX-License-Identifier: MIT
include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-tgws
PKG_VERSION:=0.1.4
PKG_RELEASE:=1
PKG_LICENSE:=MIT
PKG_MAINTAINER:=tgws-openwrt contributors

LUCI_TITLE:=LuCI support for tgws, a transparent Telegram WebSocket proxy
LUCI_DESCRIPTION:=Carries Telegram traffic of every device on the LAN over \
	WebSocket without any setup on the devices. Depends on the tgws daemon package.
LUCI_DEPENDS:=+tgws +luci-base +firewall4 +ca-bundle +curl +procd-ujail
LUCI_URL:=https://github.com/trikesh32/tgws-openwrt
LUCI_PKGARCH:=all

# Блок обязан стоять ДО include luci.mk: BuildPackage захватывает список
# немедленным присваиванием, и объявление, написанное после include, остаётся
# пустым. Без него обновление пакета затрёт /etc/config/tgws дефолтами.
define Package/luci-app-tgws/conffiles
/etc/config/tgws
endef

include $(TOPDIR)/feeds/luci/luci.mk

# Биты исполнения выставляются явно: luci.mk копирует root/ через `cp -pR`,
# то есть сохраняет права источника, а разработка идёт на Windows, где git
# записывает все файлы как 100644. Скрипт без бита исполнения procd запустить
# не может, и на роутере это выглядит как служба без единой строки в журнале.
define Build/Compile
	chmod 0600 $(PKG_BUILD_DIR)/root/etc/config/tgws
	chmod 0755 $(PKG_BUILD_DIR)/root/etc/init.d/tgws
	chmod 0755 $(PKG_BUILD_DIR)/root/etc/uci-defaults/40-luci-tgws
	chmod 0755 $(PKG_BUILD_DIR)/root/usr/libexec/tgws/gen-nft
	chmod 0755 $(PKG_BUILD_DIR)/root/usr/libexec/tgws/update-subnets
	# subnets.sh подключается через `.`, запускать его не нужно.
	chmod 0644 $(PKG_BUILD_DIR)/root/usr/libexec/tgws/subnets.sh
endef

# call BuildPackage - OpenWrt buildroot signature
