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

static void
reset_objects(void)
{
	memset(g_object, 0, sizeof(g_object));
	memset(g_release, 0, sizeof(g_release));
	memset(g_displaced, 0, sizeof(g_displaced));
	g_object_len = 0;
	g_release_len = 0;
	g_displaced_len = 0;
	g_object_present = false;
	g_release_present = false;
	g_displace_on_release_get = false;
}

static void
acquire_fresh(struct s3_client *client, struct acquire_result *out)
{
	reset_objects();
	memset(out, 0, sizeof(*out));
	s3_owner_acquire(client, "owner-test", NULL, false, acquired, out);
}

/* The marker is the only thing recovery matches on, and the scripts compare
 * this field byte for byte, so rcow_common.sh and the entrypoint are only
 * correct as long as they agree with the function that writes it.
 * test_lvs_identity.sh compares the two shell copies with each other; these
 * cases pin the C side to the same rules. */
static void
test_node_id_rejects_unsafe(struct s3_client *client)
{
	struct acquire_result res;

	assert(setenv("RCOW_NODE_ID", "bad/id", 1) == 0);
	acquire_fresh(client, &res);
	/* Rejected, not sanitised: a value the scripts would spell differently
	 * must never reach the marker. */
	assert(res.status == -EINVAL);
	assert(!res.owner_valid);
	assert(!g_object_present);
}

static void
test_node_id_rejects_too_long(struct s3_client *client)
{
	struct acquire_result res;
	char value[S3_OWNER_NODE_ID_MAX + 1];

	memset(value, 'n', S3_OWNER_NODE_ID_MAX);
	value[S3_OWNER_NODE_ID_MAX] = '\0';
	assert(setenv("RCOW_NODE_ID", value, 1) == 0);
	acquire_fresh(client, &res);
	/* An explicit value is refused rather than truncated; only the
	 * hostname fallback keeps its first S3_OWNER_NODE_ID_MAX - 1 bytes. */
	assert(res.status == -ENAMETOOLONG);
	assert(!res.owner_valid);
	assert(!g_object_present);
}

static void
test_node_id_verbatim(struct s3_client *client)
{
	struct acquire_result res;
	char value[S3_OWNER_NODE_ID_MAX];
	char needle[S3_OWNER_NODE_ID_MAX + 16];

	/* The longest value that is still accepted, using every character
	 * class the scripts allow. */
	memset(value, 'n', S3_OWNER_NODE_ID_MAX - 1);
	value[S3_OWNER_NODE_ID_MAX - 1] = '\0';
	value[0] = 'a';
	value[1] = '-';
	value[2] = '_';
	value[3] = '.';
	value[4] = '9';
	value[S3_OWNER_NODE_ID_MAX - 2] = 'Z';
	assert(strlen(value) == S3_OWNER_NODE_ID_MAX - 1);
	assert(setenv("RCOW_NODE_ID", value, 1) == 0);
	acquire_fresh(client, &res);
	assert(res.status == 0 && res.owner_valid && g_object_present);
	assert(strcmp(res.owner.node_id, value) == 0);
	snprintf(needle, sizeof(needle), "\"node_id\":\"%s\"", value);
	assert(strstr(g_object, needle) != NULL);
}

static void
test_node_id_hostname_fallback(struct s3_client *client)
{
	struct acquire_result res;
	char host[HOST_NAME_MAX + 1] = {0};
	char expect[S3_OWNER_NODE_ID_MAX] = {0};
	char needle[S3_OWNER_NODE_ID_MAX + 16];
	size_t i;

	/* The runner's hostname cannot be chosen, so derive what the rules
	 * demand of it -- first S3_OWNER_NODE_ID_MAX - 1 bytes, anything
	 * outside [A-Za-z0-9._-] replaced -- and require exactly that. */
	if (gethostname(host, sizeof(host)) != 0) {
		snprintf(host, sizeof(host), "unknown");
	}
	for (i = 0; i + 1 < sizeof(expect) && host[i] != '\0'; i++) {
		unsigned char c = (unsigned char)host[i];

		expect[i] = (isalnum(c) || c == '.' || c == '-' || c == '_') ?
			    (char)c : '_';
	}
	expect[i] = '\0';

	assert(unsetenv("RCOW_NODE_ID") == 0);
	acquire_fresh(client, &res);
	assert(res.status == 0 && res.owner_valid && g_object_present);
	assert(strcmp(res.owner.node_id, expect) == 0);
	assert(strlen(res.owner.node_id) < S3_OWNER_NODE_ID_MAX);
	for (i = 0; res.owner.node_id[i] != '\0'; i++) {
		unsigned char c = (unsigned char)res.owner.node_id[i];

		assert(isalnum(c) || c == '.' || c == '-' || c == '_');
	}
	snprintf(needle, sizeof(needle), "\"node_id\":\"%s\"", expect);
	assert(strstr(g_object, needle) != NULL);

	/* An empty value is the same as unset, not an identity of its own. */
	assert(setenv("RCOW_NODE_ID", "", 1) == 0);
	acquire_fresh(client, &res);
	assert(res.status == 0 && res.owner_valid);
	assert(strcmp(res.owner.node_id, expect) == 0);
	assert(unsetenv("RCOW_NODE_ID") == 0);
}

int
main(void)
{
	struct s3_client *client = (struct s3_client *)(uintptr_t)1;
	struct acquire_result first = {0}, second = {0}, takeover = {0};
	struct acquire_result after_release = {0}, displaced = {0};
	int release_status = INT_MIN;

	/* The acquire cases below do not depend on the identity, but the
	 * runner's environment must not decide what gets written either. */
	assert(unsetenv("RCOW_NODE_ID") == 0);

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

	test_node_id_rejects_unsafe(client);
	test_node_id_rejects_too_long(client);
	test_node_id_verbatim(client);
	test_node_id_hostname_fallback(client);

	puts("\n=== result: 5 passed, 0 failed ===");
	return 0;
}
