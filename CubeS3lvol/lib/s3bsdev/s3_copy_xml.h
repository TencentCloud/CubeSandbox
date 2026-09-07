/* Copyright (c) 2026 Tencent Inc.
 * SPDX-License-Identifier: Apache-2.0 */
/*
 * CopyObject response XML is a few hundred bytes. The client caps the body so a
 * runaway reply cannot grow without bound; these helpers decide whether that
 * prefix is a CopyObjectResult, an Error, or something too short to tell.
 *
 * Kept out of s3_client_aws.c so the delimiter arithmetic can be tested without
 * CRT, SPDK, or a bucket.
 */

#ifndef S3_COPY_XML_H
#define S3_COPY_XML_H

#include <errno.h>
#include <stdbool.h>
#include <stddef.h>
#include <string.h>

#define S3_COPY_XML_MAX 8192

static inline bool
s3_xml_has_open_tag(const char *xml, size_t len, const char *name)
{
	size_t nlen;
	size_t i;

	if (!xml || !name) {
		return false;
	}
	nlen = strlen(name);
	if (nlen == 0 || len < nlen + 2) {
		return false;
	}
	/* The character after the name is the delimiter. It must be inside the
	 * buffer: i + 1 + nlen == len would read xml[len]. */
	for (i = 0; i + 1 + nlen < len; i++) {
		char c;

		if (xml[i] != '<') {
			continue;
		}
		if (memcmp(xml + i + 1, name, nlen) != 0) {
			continue;
		}
		c = xml[i + 1 + nlen];
		if (c == '>' || c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
		    c == '/') {
			return true;
		}
	}
	return false;
}

static inline size_t
s3_copy_xml_append(char *buf, size_t cap, size_t used,
		   const void *src, size_t n, bool *truncated)
{
	size_t room;

	if (!buf || cap == 0) {
		return used;
	}
	if (used >= cap) {
		if (n > 0 && truncated) {
			*truncated = true;
		}
		return used;
	}
	if (!src || n == 0) {
		return used;
	}
	room = cap - used;
	if (n > room) {
		n = room;
		if (truncated) {
			*truncated = true;
		}
	}
	memcpy(buf + used, src, n);
	return used + n;
}

/* 0 when the prefix contains a complete CopyObjectResult opening tag.
 * -EIO for an Error tag, a missing tag, or a truncated body that never
 * formed one. An opening tag that is already complete is enough: the rest of
 * the document can be cut off at S3_COPY_XML_MAX. */
static inline int
s3_copy_xml_status(const char *xml, size_t len, bool truncated)
{
	if (s3_xml_has_open_tag(xml, len, "CopyObjectResult")) {
		return 0;
	}
	if (s3_xml_has_open_tag(xml, len, "Error")) {
		return -EIO;
	}
	(void)truncated;
	return -EIO;
}

#endif /* S3_COPY_XML_H */
