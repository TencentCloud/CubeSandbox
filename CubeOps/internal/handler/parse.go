// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package handler

import "strconv"

// parseInt / parseInt64 are thin wrappers around strconv that we centralise
// so tests can stub them if needed and so call sites stay one-liners.
func parseInt(s string) (int, error)     { return strconv.Atoi(s) }
func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
