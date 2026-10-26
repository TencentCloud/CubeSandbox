// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

// Package commands provides a set of CLI commands
package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func PrintAsJSON(x interface{}) {
	b, err := json.MarshalIndent(x, "", "    ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't marshal %+v as a JSON string: %v\n", x, err)
	}
	fmt.Println(string(b))
}

func AskForConfirm(s string, tries int) bool {
	r := bufio.NewReader(os.Stdin)

	for ; tries > 0; tries-- {
		fmt.Printf("%s [y/n]: ", s)

		res, err := r.ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			return false
		}

		if len(res) < 2 {
			continue
		}

		res = strings.ToLower(strings.TrimSpace(res))

		if res == "y" || res == "yes" {
			return true
		} else if res == "n" || res == "no" {
			return false
		}
	}

	return false
}
