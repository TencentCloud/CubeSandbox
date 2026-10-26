// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package commands

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

type Display struct {
	w *tabwriter.Writer
}

func NewDefaultTableDisplay() *Display {
	return newTableDisplay(20, 1, 3, ' ', 0)
}

func newTableDisplay(minwidth, tabwidth, padding int, padchar byte, flags uint) *Display {
	w := tabwriter.NewWriter(os.Stdout, minwidth, tabwidth, padding, padchar, flags)

	return &Display{w}
}

func (d *Display) AddRow(row []string) {
	fmt.Fprintln(d.w, strings.Join(row, "\t"))
}

func (d *Display) Flush() error {
	return d.w.Flush()
}

func (d *Display) ClearScreen() {
	fmt.Fprint(os.Stdout, "\033[2J")
	fmt.Fprint(os.Stdout, "\033[H")
}
