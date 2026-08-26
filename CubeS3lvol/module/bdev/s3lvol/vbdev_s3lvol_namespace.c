/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 *   vbdev_s3lvol_namespace -- namespace-to-S3-target registry
 *
 *   Every lvstore lives in a *namespace*, which maps to an S3 target (endpoint,
 *   bucket, region, and associated TLS / path-style settings). The registry lives
 *   in process memory -- it is populated by the startup script through
 *   rcow_add_s3_config and does not survive a restart, because the same startup
 *   script repopulates it every time.
 *
 *   For now the mapping is a direct TAILQ lookup. That will be replaced by
 *   whatever the namespace service provides, the callers are already going
 *   through rcow_namespace_to_target() and will not have to change.
 */

#include "spdk/stdinc.h"
#include "spdk/log.h"
#include "spdk/queue.h"
#include "spdk/string.h"
#include "s3lvol/s3_types.h"

#include "vbdev_s3lvol.h"

SPDK_LOG_REGISTER_COMPONENT(s3lvol_ns)

struct rcow_namespace {
	char                *name;
	struct s3_target     target;
	TAILQ_ENTRY(rcow_namespace) link;
};

static TAILQ_HEAD(, rcow_namespace) g_ns_list = TAILQ_HEAD_INITIALIZER(g_ns_list);

static struct rcow_namespace *
rcow_namespace_find(const char *name)
{
	struct rcow_namespace *ns;

	if (!name) {
		return NULL;
	}
	TAILQ_FOREACH(ns, &g_ns_list, link) {
		if (strcmp(ns->name, name) == 0) {
			return ns;
		}
	}
	return NULL;
}

int
rcow_namespace_add(const char *name, const struct s3_target *target)
{
	struct rcow_namespace *ns;
	struct s3_target copy = {0};

	if (!name || !name[0] || !target) {
		return -EINVAL;
	}

	if (rcow_namespace_find(name)) {
		SPDK_ERRLOG("namespace '%s' already registered\n", name);
		return -EEXIST;
	}

	ns = calloc(1, sizeof(*ns));
	if (!ns) {
		return -ENOMEM;
	}

	/* Copy only the fields that matter for the S3 connection. Credential
	 * fields (access_key / secret_key / session_token) are populated from the
	 * environment inside s3_client_get_or_create(), never from the RPC.
	 *
	 * endpoint and bucket are required rather than optional: a namespace that
	 * resolved to a target without them would only fail later, inside a client
	 * that cannot say which namespace sent it there. */
	ns->name      = strdup(name);
	copy.endpoint = target->endpoint ? strdup(target->endpoint) : NULL;
	copy.bucket   = target->bucket   ? strdup(target->bucket)   : NULL;
	copy.region   = target->region   ? strdup(target->region)   : NULL;
	copy.prefix   = target->prefix   ? strdup(target->prefix)   : NULL;

	if (!ns->name || !copy.endpoint || !copy.bucket ||
	    (target->region && !copy.region) ||
	    (target->prefix && !copy.prefix)) {
		free(copy.prefix);
		free(copy.region);
		free(copy.bucket);
		free(copy.endpoint);
		free(ns->name);
		free(ns);
		return -ENOMEM;
	}

	copy.auth_mode      = S3_AUTH_ENV;
	copy.use_path_style = target->use_path_style;
	copy.verify_tls     = target->verify_tls;

	ns->target = copy;

	TAILQ_INSERT_TAIL(&g_ns_list, ns, link);
	SPDK_NOTICELOG("added namespace '%s': endpoint=%s bucket=%s\n",
		       name, copy.endpoint, copy.bucket);
	return 0;
}

const struct s3_target *
rcow_namespace_to_target(const char *name)
{
	struct rcow_namespace *ns = rcow_namespace_find(name);

	return ns ? &ns->target : NULL;
}

void
rcow_namespace_for_each(rcow_ns_iter_fn fn, void *ctx)
{
	struct rcow_namespace *ns;

	TAILQ_FOREACH(ns, &g_ns_list, link) {
		fn(ns->name, &ns->target, ctx);
	}
}
