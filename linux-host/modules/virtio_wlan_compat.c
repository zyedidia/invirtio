// SPDX-License-Identifier: GPL-2.0
/*
 * Builds virtio_wlan.c against the 6.12 kernel, whose cfg80211 operations for
 * keys and stations take a net_device where those of 7.2 take a
 * wireless_dev. The driver's operations table gets a type with the 7.2
 * signatures, and cfg80211 gets a table whose operations call it.
 */

#include <linux/debugfs.h>
#include <linux/etherdevice.h>
#include <linux/module.h>
#include <linux/netdevice.h>
#include <linux/virtio.h>
#include <linux/virtio_config.h>
#include <linux/virtio_net.h>
#include <linux/virtio_wlan.h>
#include <net/cfg80211.h>

struct virtio_wlan_ops_7_2 {
	int (*scan)(struct wiphy *wiphy, struct cfg80211_scan_request *request);
	void (*abort_scan)(struct wiphy *wiphy, struct wireless_dev *wdev);
	int (*connect)(struct wiphy *wiphy, struct net_device *dev,
		       struct cfg80211_connect_params *sme);
	int (*disconnect)(struct wiphy *wiphy, struct net_device *dev,
			  u16 reason_code);
	int (*add_key)(struct wiphy *wiphy, struct wireless_dev *wdev,
		       int link_id, u8 key_index, bool pairwise,
		       const u8 *mac_addr, struct key_params *params);
	int (*del_key)(struct wiphy *wiphy, struct wireless_dev *wdev,
		       int link_id, u8 key_index, bool pairwise,
		       const u8 *mac_addr);
	int (*set_default_key)(struct wiphy *wiphy, struct net_device *netdev,
			       int link_id, u8 key_index, bool unicast,
			       bool multicast);
	int (*set_default_mgmt_key)(struct wiphy *wiphy,
				    struct wireless_dev *wdev, int link_id,
				    u8 key_index);
	int (*change_station)(struct wiphy *wiphy, struct wireless_dev *wdev,
			      const u8 *mac, struct station_parameters *params);
	int (*get_station)(struct wiphy *wiphy, struct wireless_dev *wdev,
			   const u8 *mac, struct station_info *sinfo);
	int (*dump_station)(struct wiphy *wiphy, struct wireless_dev *wdev,
			    int idx, u8 *mac, struct station_info *sinfo);
};

static const struct cfg80211_ops virtio_wlan_ops_6_12;

#define cfg80211_ops virtio_wlan_ops_7_2
#define wiphy_new(ops, size) wiphy_new(&virtio_wlan_ops_6_12, size)
#include "virtio_wlan.c"
#undef wiphy_new
#undef cfg80211_ops

static int virtio_wlan_add_key_6_12(struct wiphy *wiphy,
				    struct net_device *dev, int link_id,
				    u8 idx, bool pairwise, const u8 *mac,
				    struct key_params *params)
{
	return virtio_wlan_cfg80211_ops.add_key(wiphy, dev->ieee80211_ptr,
						link_id, idx, pairwise, mac,
						params);
}

static int virtio_wlan_del_key_6_12(struct wiphy *wiphy,
				    struct net_device *dev, int link_id,
				    u8 idx, bool pairwise, const u8 *mac)
{
	return virtio_wlan_cfg80211_ops.del_key(wiphy, dev->ieee80211_ptr,
						link_id, idx, pairwise, mac);
}

static int virtio_wlan_set_default_mgmt_key_6_12(struct wiphy *wiphy,
						 struct net_device *dev,
						 int link_id, u8 idx)
{
	return virtio_wlan_cfg80211_ops.set_default_mgmt_key(wiphy,
			dev->ieee80211_ptr, link_id, idx);
}

static int virtio_wlan_change_station_6_12(struct wiphy *wiphy,
					   struct net_device *dev,
					   const u8 *mac,
					   struct station_parameters *params)
{
	return virtio_wlan_cfg80211_ops.change_station(wiphy,
			dev->ieee80211_ptr, mac, params);
}

static int virtio_wlan_get_station_6_12(struct wiphy *wiphy,
					struct net_device *dev, const u8 *mac,
					struct station_info *sinfo)
{
	return virtio_wlan_cfg80211_ops.get_station(wiphy, dev->ieee80211_ptr,
						    mac, sinfo);
}

static int virtio_wlan_dump_station_6_12(struct wiphy *wiphy,
					 struct net_device *dev, int idx,
					 u8 *mac, struct station_info *sinfo)
{
	return virtio_wlan_cfg80211_ops.dump_station(wiphy, dev->ieee80211_ptr,
						     idx, mac, sinfo);
}

static const struct cfg80211_ops virtio_wlan_ops_6_12 = {
	.scan = virtio_wlan_scan,
	.abort_scan = virtio_wlan_abort_scan,
	.connect = virtio_wlan_connect,
	.disconnect = virtio_wlan_disconnect,
	.add_key = virtio_wlan_add_key_6_12,
	.del_key = virtio_wlan_del_key_6_12,
	.set_default_key = virtio_wlan_set_default_key,
	.set_default_mgmt_key = virtio_wlan_set_default_mgmt_key_6_12,
	.change_station = virtio_wlan_change_station_6_12,
	.get_station = virtio_wlan_get_station_6_12,
	.dump_station = virtio_wlan_dump_station_6_12,
};
