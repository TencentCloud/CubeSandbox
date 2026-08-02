// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

type manifest struct {
	Subprotocol  string    `json:"subprotocol"`
	GrantPrefix  string    `json:"grantPrefix"`
	Channels     []channel `json:"channels"`
	Limits       limits    `json:"limits"`
	ErrorCodes   []code    `json:"errorCodes"`
	CloseReasons []code    `json:"closeReasons"`
	ReasonCodes  []string  `json:"reasonCodes"`
}

type channel struct {
	Name   string `json:"name"`
	TSName string `json:"tsName"`
	Value  byte   `json:"value"`
}

type limits struct {
	MaxPayloadBytes int `json:"maxPayloadBytes"`
	MaxStatusBytes  int `json:"maxStatusBytes"`
	MinDimension    int `json:"minDimension"`
	MaxDimension    int `json:"maxDimension"`
}

type code struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type output struct {
	path   string
	source string
	data   []byte
}

func main() {
	check := flag.Bool("check", false, "verify generated files are current")
	flag.Parse()

	root := repositoryRoot()
	data, err := os.ReadFile(filepath.Join(root, "protocol/terminal-v1.json"))
	checkErr(err)
	var spec manifest
	checkErr(json.Unmarshal(data, &spec))
	checkErr(validate(spec))

	outputs := []output{
		goOutput(root, "CubeOps/internal/terminal/protocol.go", "CubeOps/internal/terminal/protocol_generated.go", "terminal", spec, false),
		goOutput(root, "CubeMaster/pkg/terminalprotocol/protocol.go", "CubeMaster/pkg/terminalprotocol/protocol_generated.go", "terminalprotocol", spec, false),
		goOutput(root, "Cubelet/services/cubebox/terminalcore/types.go", "Cubelet/services/cubebox/terminalcore/protocol_generated.go", "terminalcore", spec, true),
		{
			path:   filepath.Join(root, "web/src/lib/terminal/protocol.generated.ts"),
			source: filepath.Join(root, "web/src/lib/terminal/protocol.ts"),
			data:   tsOutput(spec),
		},
	}
	for _, generated := range outputs {
		if _, err := os.Stat(generated.source); errors.Is(err, os.ErrNotExist) {
			continue
		} else {
			checkErr(err)
		}
		if *check {
			current, err := os.ReadFile(generated.path)
			checkErr(err)
			if !bytes.Equal(current, generated.data) {
				fmt.Fprintf(os.Stderr, "%s is stale; run make terminal-protocol-sync\n", generated.path)
				os.Exit(1)
			}
			continue
		}
		checkErr(os.WriteFile(generated.path, generated.data, 0o644))
	}
}

func validate(spec manifest) error {
	if spec.Subprotocol == "" || spec.GrantPrefix == "" || len(spec.Channels) == 0 || len(spec.ErrorCodes) == 0 || len(spec.CloseReasons) == 0 {
		return fmt.Errorf("terminal protocol manifest is incomplete")
	}
	if spec.Limits.MaxPayloadBytes <= 0 || spec.Limits.MaxStatusBytes <= 0 || spec.Limits.MinDimension <= 0 || spec.Limits.MaxDimension < spec.Limits.MinDimension {
		return fmt.Errorf("terminal protocol limits are invalid")
	}
	goNames, tsNames, channelValues := map[string]struct{}{}, map[string]struct{}{}, map[byte]struct{}{}
	for _, channel := range spec.Channels {
		if !goIdentifier.MatchString(channel.Name) || !tsIdentifier.MatchString(channel.TSName) {
			return fmt.Errorf("terminal protocol channel name is invalid: %q/%q", channel.Name, channel.TSName)
		}
		if duplicate(goNames, channel.Name) || duplicate(tsNames, channel.TSName) || duplicateByte(channelValues, channel.Value) {
			return fmt.Errorf("terminal protocol channel is duplicated: %q", channel.Name)
		}
	}
	errorValues, closeValues, reasons := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	if err := validateCodes("error code", spec.ErrorCodes, errorValues); err != nil {
		return err
	}
	if err := validateCodes("close reason", spec.CloseReasons, closeValues); err != nil {
		return err
	}
	for _, reason := range spec.ReasonCodes {
		if !wireValue.MatchString(reason) || duplicate(reasons, reason) {
			return fmt.Errorf("terminal protocol reason code is invalid or duplicated: %q", reason)
		}
	}
	for value := range errorValues {
		if _, ok := reasons[value]; !ok {
			return fmt.Errorf("terminal error code %q is missing from reasonCodes", value)
		}
	}
	for value := range closeValues {
		if _, ok := reasons[value]; !ok {
			return fmt.Errorf("terminal close reason %q is missing from reasonCodes", value)
		}
	}
	return nil
}

var (
	goIdentifier = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	tsIdentifier = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	wireValue    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

func validateCodes(kind string, values []code, seenValues map[string]struct{}) error {
	seenNames := make(map[string]struct{}, len(values))
	for _, item := range values {
		if !goIdentifier.MatchString(item.Name) || !wireValue.MatchString(item.Value) || duplicate(seenNames, item.Name) || duplicate(seenValues, item.Value) {
			return fmt.Errorf("terminal protocol %s is invalid or duplicated: %q/%q", kind, item.Name, item.Value)
		}
	}
	return nil
}

func duplicate(values map[string]struct{}, value string) bool {
	_, exists := values[value]
	values[value] = struct{}{}
	return exists
}

func duplicateByte(values map[byte]struct{}, value byte) bool {
	_, exists := values[value]
	values[value] = struct{}{}
	return exists
}

func repositoryRoot() string {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		checkErr(fmt.Errorf("locate terminal protocol generator source"))
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
}

func goOutput(root, source, path, packageName string, spec manifest, cubelet bool) output {
	var b bytes.Buffer
	fmt.Fprintln(&b, "// Code generated by terminal-protocol-gen. DO NOT EDIT.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "package %s\n\n", packageName)
	if !cubelet {
		fmt.Fprintln(&b, "const (")
		fmt.Fprintf(&b, "\tSubprotocol = %s\n", strconv.Quote(spec.Subprotocol))
		fmt.Fprintf(&b, "\tGrantPrefix = %s\n\n", strconv.Quote(spec.GrantPrefix))
		for _, channel := range spec.Channels {
			fmt.Fprintf(&b, "\tChannel%s byte = 0x%02x\n", channel.Name, channel.Value)
		}
		fmt.Fprintf(&b, "\n\tMaxTerminalPayloadBytes = %d\n", spec.Limits.MaxPayloadBytes)
		fmt.Fprintf(&b, "\tmaxStatusBytes = %d\n", spec.Limits.MaxStatusBytes)
		fmt.Fprintf(&b, "\tminTerminalDimension = %d\n", spec.Limits.MinDimension)
		fmt.Fprintf(&b, "\tmaxTerminalDimension = %d\n", spec.Limits.MaxDimension)
		fmt.Fprintln(&b, ")")
	}
	writeGoCodes(&b, spec, cubelet)
	formatted, err := format.Source(b.Bytes())
	checkErr(err)
	return output{
		path:   filepath.Join(root, path),
		source: filepath.Join(root, source),
		data:   formatted,
	}
}

func writeGoCodes(b *bytes.Buffer, spec manifest, cubelet bool) {
	if cubelet {
		fmt.Fprintln(b, "const (")
		for _, item := range spec.ErrorCodes {
			fmt.Fprintf(b, "\tCode%s = %s\n", item.Name, strconv.Quote(item.Value))
		}
		fmt.Fprintln(b, ")\n\nconst (")
		for _, item := range spec.CloseReasons {
			fmt.Fprintf(b, "\tClose%s = %s\n", item.Name, strconv.Quote(item.Value))
		}
		fmt.Fprintln(b, ")")
		writeGoSet(b, "terminalCloseReasons", spec.CloseReasons)
		return
	}
	writeGoSet(b, "terminalErrorCodes", spec.ErrorCodes)
	writeGoSet(b, "terminalCloseReasons", spec.CloseReasons)
}

func writeGoSet(b *bytes.Buffer, name string, values []code) {
	fmt.Fprintf(b, "\nvar %s = map[string]struct{}{\n", name)
	for _, item := range values {
		fmt.Fprintf(b, "\t%s: {},\n", strconv.Quote(item.Value))
	}
	fmt.Fprintln(b, "}")
}

func tsOutput(spec manifest) []byte {
	var b bytes.Buffer
	fmt.Fprintln(&b, "// Code generated by terminal-protocol-gen. DO NOT EDIT.")
	fmt.Fprintf(&b, "export const TERMINAL_SUBPROTOCOL = %s;\n", quoteTS(spec.Subprotocol))
	fmt.Fprintf(&b, "export const TERMINAL_GRANT_PREFIX = %s;\n\n", quoteTS(spec.GrantPrefix))
	fmt.Fprintln(&b, "export const TERMINAL_CHANNEL = {")
	for _, channel := range spec.Channels {
		fmt.Fprintf(&b, "  %s: 0x%02x,\n", channel.TSName, channel.Value)
	}
	fmt.Fprintln(&b, "} as const;")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "export const MAX_TERMINAL_PAYLOAD_BYTES = %d;\n", spec.Limits.MaxPayloadBytes)
	fmt.Fprintf(&b, "export const MAX_TERMINAL_STATUS_BYTES = %d;\n", spec.Limits.MaxStatusBytes)
	fmt.Fprintf(&b, "export const MIN_TERMINAL_DIMENSION = %d;\n", spec.Limits.MinDimension)
	fmt.Fprintf(&b, "export const MAX_TERMINAL_DIMENSION = %d;\n", spec.Limits.MaxDimension)
	writeTSArray(&b, "TERMINAL_ERROR_CODES", codeValues(spec.ErrorCodes))
	writeTSArray(&b, "TERMINAL_CLOSE_REASONS", codeValues(spec.CloseReasons))
	writeTSArray(&b, "TERMINAL_REASON_CODES", spec.ReasonCodes)
	return b.Bytes()
}

func writeTSArray(b *bytes.Buffer, name string, values []string) {
	fmt.Fprintf(b, "\nexport const %s = [\n", name)
	for _, value := range values {
		fmt.Fprintf(b, "  %s,\n", quoteTS(value))
	}
	fmt.Fprintln(b, "] as const;")
}

func quoteTS(value string) string {
	var result strings.Builder
	result.WriteByte('\'')
	for _, char := range value {
		switch char {
		case '\\':
			result.WriteString(`\\`)
		case '\'':
			result.WriteString(`\'`)
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		case '\u2028', '\u2029':
			fmt.Fprintf(&result, `\u%04x`, char)
		default:
			if char < 0x20 || char == 0x7f {
				fmt.Fprintf(&result, `\u%04x`, char)
			} else {
				result.WriteRune(char)
			}
		}
	}
	result.WriteByte('\'')
	return result.String()
}

func codeValues(values []code) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Value)
	}
	return result
}

func checkErr(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
