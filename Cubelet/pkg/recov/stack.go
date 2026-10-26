// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
Package recov support recover handler
* Copyright (c) 2020 Tencent Serverless
* All rights reserved
* Author: jiangdu
* Date: 2020-06-08
*/
package recov

import (
	"bytes"
	"fmt"
	"go/build"
	"path/filepath"
	"runtime"
	"strings"
)

var trimPaths []string

func init() {
	for _, prefix := range build.Default.SrcDirs() {
		if prefix[len(prefix)-1] != filepath.Separator {
			prefix += string(filepath.Separator)
		}
		trimPaths = append(trimPaths, prefix)
	}
}

func trimPath(filename string) string {
	for _, prefix := range trimPaths {
		if trimmed := strings.TrimPrefix(filename, prefix); len(trimmed) < len(filename) {
			return trimmed
		}
	}
	return filename
}

func functionName(pc uintptr) string {
	fn := runtime.FuncForPC(pc)
	if fn == nil {
		return "unknown"
	}
	return fn.Name()
}

func DumpStacktrace(skip int, attach interface{}) string {
	buf := bytes.NewBuffer([]byte{})
	buf.WriteString(fmt.Sprintf("%v", attach))
	buf.WriteRune('\n')
	for i := skip; ; i++ {
		pc, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		file = trimPath(file)
		name := functionName(pc)
		if name == "runtime.goexit" {
			continue
		}
		buf.WriteString(fmt.Sprintf("at %s(%s:%d)\n", name, file, line))
	}
	return buf.String()
}
