// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package profile

import "github.com/prometheus/client_golang/prometheus"

// Plugin duration stage labels. They are the kind label of
// scheduler_plugin_duration_seconds and must stay stable: dashboards and the
// scheduler package's metric helpers key off these exact strings.
const (
	PluginKindGuard  = "guard"
	PluginKindFilter = "filter"
	PluginKindScore  = "score"
)

// ResolvePluginObserver turns a compiled binding into the histogram series it
// should record into. The scheduler package installs it at init, because the
// histogram itself lives there and this package cannot import it back.
// Nil means observers stay unset and callers fall back to a label lookup.
var ResolvePluginObserver func(profileName, pluginName, kind string) prometheus.Observer

func pluginObserver(profileName, pluginName, kind string) prometheus.Observer {
	if ResolvePluginObserver == nil || profileName == "" || pluginName == "" || kind == "" {
		return nil
	}
	return ResolvePluginObserver(profileName, pluginName, kind)
}
