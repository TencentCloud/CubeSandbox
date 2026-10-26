// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package image

import (
	"bytes"
	"encoding/json"
	"fmt"
	"text/template"

	"github.com/sirupsen/logrus"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func builtinTmplFuncs() template.FuncMap {
	t := cases.Title(language.Und, cases.NoLower)
	l := cases.Lower(language.Und)
	u := cases.Upper(language.Und)

	return template.FuncMap{
		outputTypeJSON: jsonBuiltinTmplFunc,
		"title":        t.String,
		"lower":        l.String,
		"upper":        u.String,
	}
}

func jsonBuiltinTmplFunc(v interface{}) string {
	o := new(bytes.Buffer)

	enc := json.NewEncoder(o)
	if err := enc.Encode(v); err != nil {
		logrus.Fatalf("Unable to encode JSON: %v", err)
	}

	return o.String()
}

func tmplExecuteRawJSON(tmplStr, rawJSON string) (string, error) {
	dec := json.NewDecoder(
		bytes.NewReader([]byte(rawJSON)),
	)
	dec.UseNumber()

	var raw interface{}
	if err := dec.Decode(&raw); err != nil {
		return "", fmt.Errorf("failed to decode json: %w", err)
	}

	o := new(bytes.Buffer)

	tmpl, err := template.New("tmplExecuteRawJSON").Funcs(builtinTmplFuncs()).Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("failed to generate go-template: %w", err)
	}

	tmpl = tmpl.Option("missingkey=error")
	if err := tmpl.Execute(o, raw); err != nil {
		return "", fmt.Errorf("failed to template data: %w", err)
	}

	return o.String(), nil
}
