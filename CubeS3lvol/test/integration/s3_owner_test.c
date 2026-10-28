/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */

#include "spdk/stdinc.h"

#include "s3lvol/s3_owner.h"

static char g_object[S3_OWNER_DOC_MAX];
static size_t g_object_len;
static bool g_object_present;
static char g_release[S3_OWNER_DOC_MAX];
static size_t g_release_len;
static bool g_release_present;
static bool g_displace_on_release_get;
static char g_displaced[S3_OWNER_DOC_MAX];
static size_t g_displaced_len;

int
s3_get_range(struct s3_client *client, const char *key, uint64_t offset,
	     uint64_t length, void *buf, s3_get_cb cb_fn, void *cb_arg)
{
	size_t copied;
	bool release = strstr(key, "owner-released/") != NULL;
	const char *object = release ? g_release : g_object;
	size_t object_len = release ? g_release_len : g_object_len;
	bool present = release ? g_release_present : g_object_present;

	(void)client;
	if (!present) {
		cb_fn(cb_arg, 0, -ENOENT);
		return 0;
	}
	if (release && g_displace_on_release_get) {
		char *nonce = strstr(g_object, "\"nonce\":\"");

		g_displace_on_release_get = false;
		if (nonce != NULL) {
			nonce += strlen("\"nonce\":\"");
			*nonce = (*nonce == '0') ? '1' : '0';
		}
		memcpy(g_displaced, g_object, g_object_len);
		g_displaced_len = g_object_len;
	}
	copied = (size_t)length < object_len - (size_t)offset ?
		 (size_t)length : object_len - (size_t)offset;
	memcpy(buf, object + offset, copied);
	cb_fn(cb_arg, copied, 0);
	return 0;
}

int
s3_put(struct s3_client *client, const char *key, struct iovec *iov,
       int iovcnt, bool if_none_match, s3_op_cb cb_fn, void *cb_arg)
{
	bool release = strstr(key, "owner-released/") != NULL;
	char *object = release ? g_release : g_object;

	(void)client;
	(void)if_none_match;
	assert(iovcnt == 1);
	assert(iov[0].iov_len <= sizeof(g_object));
	memcpy(object, iov[0].iov_base, iov[0].iov_len);
	if (release) {
		g_release_len = iov[0].iov_len;
		g_release_present = true;
	} else {
		g_object_len = iov[0].iov_len;
		g_object_present = true;
	}
	cb_fn(cb_arg, 0);
	return 0;
}

int
s3_delete(struct s3_client *client, const char *key, s3_op_cb cb_fn,
	  void *cb_arg)
{
	bool release = strstr(key, "owner-released/") != NULL;

	(void)client;
	if (release) {
		g_release_present = false;
		g_release_len = 0;
	} else {
		g_object_present = false;
		g_object_len = 0;
	}
	cb_fn(cb_arg, 0);
	return 0;
}

struct acquire_result {
	struct s3_owner_info owner;
	bool owner_valid;
	int status;
};

static void
acquired(void *cb_arg, const struct s3_owner_info *owner, int status)
{
	struct acquire_result *result = cb_arg;

	result->status = status;
	if (owner) {
		result->owner = *owner;
		result->owner_valid = true;
	}
}

static void
released(void *cb_arg, int status)
{
	*(int *)cb_arg = status;
}

int
main(void)
{
	struct s3_client *client = (struct s3_client *)(uintptr_t)1;
	struct acquire_result first = {0}, second = {0}, takeover = {0};
	struct acquire_result after_release = {0}, displaced = {0};
	int release_status = INT_MIN;

	s3_owner_acquire(client, "owner-test", NULL, false, acquired, &first);
	assert(first.status == 0 && first.owner_valid && g_object_present);

	/* A second attach without force sees the first owner and is refused. */
	s3_owner_acquire(client, "owner-test", NULL, false, acquired, &second);
	assert(second.status == -EBUSY && second.owner_valid);
	assert(spdk_uuid_compare(&second.owner.nonce, &first.owner.nonce) == 0);

	/* Explicit force takeover installs and returns a fresh identity. */
	s3_owner_acquire(client, "owner-test", NULL, true, acquired, &takeover);
	assert(takeover.status == 0 && takeover.owner_valid);
	assert(spdk_uuid_compare(&takeover.owner.nonce, &first.owner.nonce) != 0);

	/* The displaced owner may finish late. It must not delete the takeover. */
	s3_owner_release(client, "owner-test", &first.owner, released,
			 &release_status);
	assert(release_status == -ESTALE);
	assert(g_object_present);
	assert(!g_release_present);

	release_status = INT_MIN;
	s3_owner_release(client, "owner-test", &takeover.owner, released,
			 &release_status);
	assert(release_status == 0);
	assert(g_object_present && g_release_present);

	/* A matching nonce-specific release proof makes a clean restart automatic,
	 * without deleting a marker that a force takeover may have replaced. */
	s3_owner_acquire(client, "owner-test", NULL, false, acquired,
			 &after_release);
	assert(after_release.status == 0 && after_release.owner_valid);
	assert(spdk_uuid_compare(&after_release.owner.nonce,
				 &takeover.owner.nonce) != 0);

	/* Release again, then change the marker after the proof is read and
	 * before the takeover PUT. The stale acquire must not overwrite it. */
	release_status = INT_MIN;
	s3_owner_release(client, "owner-test", &after_release.owner, released,
			 &release_status);
	assert(release_status == 0);
	g_displace_on_release_get = true;
	s3_owner_acquire(client, "owner-test", NULL, false, acquired, &displaced);
	assert(displaced.status == -EBUSY);
	assert(g_displaced_len == g_object_len);
	assert(memcmp(g_object, g_displaced, g_displaced_len) == 0);

	puts("\n=== result: 1 passed, 0 failed ===");
	return 0;
}
