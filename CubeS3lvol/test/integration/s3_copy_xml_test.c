/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * CopyObject XML helpers: delimiter bounds, truncated bodies, fragmented
 * callbacks. No S3, no SPDK -- the arithmetic is the defect.
 */

#include "s3_copy_xml.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int g_pass, g_fail;

static void
check_true(const char *what, bool ok)
{
	if (ok) {
		g_pass++;
		printf("\t[PASS] %s\n", what);
	} else {
		g_fail++;
		printf("\t[FAIL] %s\n", what);
	}
}

static void
check_int(const char *what, int got, int want)
{
	if (got == want) {
		g_pass++;
		printf("\t[PASS] %s (got %d, want %d)\n", what, got, want);
	} else {
		g_fail++;
		printf("\t[FAIL] %s (got %d, want %d)\n", what, got, want);
	}
}

static void
check_size(const char *what, size_t got, size_t want)
{
	if (got == want) {
		g_pass++;
		printf("\t[PASS] %s (got %zu, want %zu)\n", what, got, want);
	} else {
		g_fail++;
		printf("\t[FAIL] %s (got %zu, want %zu)\n", what, got, want);
	}
}

int
main(void)
{
	char buf[S3_COPY_XML_MAX];
	char *asan;
	const char *ok =
		"<?xml version=\"1.0\"?><CopyObjectResult><ETag>\"a\"</ETag></CopyObjectResult>";
	const char *err =
		"<?xml version=\"1.0\"?><Error><Code>InternalError</Code></Error>";
	const char *attrs = "<CopyObjectResult xmlns=\"s3\"><ETag>e</ETag></CopyObjectResult>";
	size_t i;
	size_t used;
	bool truncated;

	printf("[1] opening-tag delimiter must sit inside the buffer\n");
	check_true("CopyObjectResult",
		   s3_xml_has_open_tag(ok, strlen(ok), "CopyObjectResult"));
	check_true("Error", s3_xml_has_open_tag(err, strlen(err), "Error"));
	check_true("tag with attributes",
		   s3_xml_has_open_tag(attrs, strlen(attrs), "CopyObjectResult"));
	check_true("empty is not a tag",
		   !s3_xml_has_open_tag("", 0, "CopyObjectResult"));
	check_true("NULL xml", !s3_xml_has_open_tag(NULL, 8, "Error"));
	check_true("too short for <Error>",
		   !s3_xml_has_open_tag("<Error", strlen("<Error"), "Error"));
	check_true("<Error> complete",
		   s3_xml_has_open_tag("<Error>", strlen("<Error>"), "Error"));
	check_true("self-closing",
		   s3_xml_has_open_tag("<Error/>", strlen("<Error/>"), "Error"));

	/* ASan-reproduced case: 8192 bytes ending in "<Error" with no delimiter.
	 * The old loop allowed i + 1 + nlen == len and read xml[len]. */
	asan = malloc(S3_COPY_XML_MAX);
	if (!asan) {
		fprintf(stderr, "malloc failed\n");
		return 1;
	}
	memset(asan, 'x', S3_COPY_XML_MAX);
	memcpy(asan + S3_COPY_XML_MAX - 6, "<Error", 6);
	check_true("8192-byte body ending in <Error is not a tag",
		   !s3_xml_has_open_tag(asan, S3_COPY_XML_MAX, "Error"));
	check_true("8192-byte body ending in <Error is not CopyObjectResult",
		   !s3_xml_has_open_tag(asan, S3_COPY_XML_MAX, "CopyObjectResult"));
	check_int("that truncated body is -EIO",
		  s3_copy_xml_status(asan, S3_COPY_XML_MAX, true), -EIO);

	memcpy(asan + S3_COPY_XML_MAX - 7, "<Error>", 7);
	check_true("8192-byte body ending in <Error> is a tag",
		   s3_xml_has_open_tag(asan, S3_COPY_XML_MAX, "Error"));
	check_int("complete Error at the last byte is -EIO",
		  s3_copy_xml_status(asan, S3_COPY_XML_MAX, false), -EIO);
	free(asan);

	printf("[2] incomplete XML is not success\n");
	check_int("empty body", s3_copy_xml_status("", 0, false), -EIO);
	check_int("garbage", s3_copy_xml_status("not xml", 7, false), -EIO);
	check_int("partial CopyObjectResult",
		  s3_copy_xml_status("<CopyObjectResult",
				     strlen("<CopyObjectResult"), false), -EIO);
	check_int("complete result",
		  s3_copy_xml_status(ok, strlen(ok), false), 0);
	check_int("Error body", s3_copy_xml_status(err, strlen(err), false), -EIO);
	check_true("Errorish is not Error",
		   !s3_xml_has_open_tag("<Errorish><Code>x</Code></Errorish>",
				       strlen("<Errorish><Code>x</Code></Errorish>"),
				       "Error"));
	check_int("Errorish is not an Error body",
		  s3_copy_xml_status("<Errorish><Code>x</Code></Errorish>",
				     strlen("<Errorish><Code>x</Code></Errorish>"),
				     false), -EIO);
	check_int("CopyObjectResult wins when Error is also present",
		  s3_copy_xml_status("<CopyObjectResult/><Error><Code>x</Code></Error>",
				     strlen("<CopyObjectResult/><Error><Code>x</Code></Error>"),
				     false), 0);
	check_int("truncated without a tag",
		  s3_copy_xml_status("xxxx", 4, true), -EIO);
	check_int("truncated after a complete result tag is still success",
		  s3_copy_xml_status(ok, strlen(ok), true), 0);

	printf("[3] callback fragments assemble, overflow is truncated\n");
	memset(buf, 0, sizeof(buf));
	used = 0;
	truncated = false;
	for (i = 0; i < strlen(ok); i++) {
		used = s3_copy_xml_append(buf, sizeof(buf), used, ok + i, 1,
					  &truncated);
	}
	check_size("one-byte fragments fill the document", used, strlen(ok));
	check_true("one-byte fragments were not truncated", !truncated);
	check_int("assembled CopyObjectResult",
		  s3_copy_xml_status(buf, used, truncated), 0);

	used = 0;
	truncated = false;
	used = s3_copy_xml_append(buf, 8, used, "abcdefghijkl", 12, &truncated);
	check_size("overflow keeps the cap", used, 8);
	check_true("overflow sets truncated", truncated);
	check_int("truncated prefix without a tag",
		  s3_copy_xml_status(buf, used, truncated), -EIO);

	truncated = false;
	used = s3_copy_xml_append(buf, 8, 8, "more", 4, &truncated);
	check_size("append into a full buffer stays full", used, 8);
	check_true("append into a full buffer is truncated", truncated);

	printf("\n=== %d passed, %d failed ===\n", g_pass, g_fail);
	return g_fail ? 1 : 0;
}
