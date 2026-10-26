#ifndef S3LVOL_S3_BUILD_INFO_H
#define S3LVOL_S3_BUILD_INFO_H

#include <stddef.h>

/* Injected by mk/s3lvol.common.mk. The fallbacks exist so a plain `make` in a
 * tree without git metadata still builds. */
#ifndef S3LVOL_VERSION
#define S3LVOL_VERSION "unknown"
#endif
#ifndef S3LVOL_GIT_COMMIT
#define S3LVOL_GIT_COMMIT "unknown"
#endif
#ifndef S3LVOL_SPDK_VERSION
#define S3LVOL_SPDK_VERSION "unknown"
#endif

/* RPC surface version. Bump when a rcow_* method's request or reply shape
 * changes; callers negotiate on it, so a bump is a promise that N-1 callers
 * keep working. Independent of the on-disk format versions below. */
#define S3LVOL_RPC_API_VERSION 1

/* The rendered document is a fixed set of small fields; 512 leaves room for the
 * version strings while staying small enough to sit on the caller's stack. */
#define S3LVOL_BUILD_INFO_MAX 512

/* Renders the build description as one JSON object. Returns the number of
 * bytes written excluding the terminator, or -ENOSPC if it does not fit. */
int s3lvol_build_info_json(char *buf, size_t len);

#endif /* S3LVOL_S3_BUILD_INFO_H */
