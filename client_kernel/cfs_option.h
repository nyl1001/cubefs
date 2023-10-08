#ifndef __CFS_OPTION_H__
#define __CFS_OPTION_H__

#include "cfs_common.h"

struct cfs_options {
	struct sockaddr_storage_array addrs;
	char *volume;
	char *path;
	char *owner;
	u32 dentry_cache_valid_ms;
	u32 attr_cache_valid_ms;
	u32 quota_cache_valid_ms;
	bool enable_quota;
};

struct cfs_options *cfs_options_new(const char *dev_str, const char *opt_str);
void cfs_options_release(struct cfs_options *options);

#endif
