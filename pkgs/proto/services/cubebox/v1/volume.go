// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

package cubebox

// AnnotationPluginVolumeSources carries the driver and state read by Master
// from volume records.
const AnnotationPluginVolumeSources = "plugin-volume-sources"

// ExtPausePluginVolumeIDs is the complete JSON list of plugin volumes in a
// pause package, including volumes still referenced by the node after Pause.
const ExtPausePluginVolumeIDs = "cube-pause-plugin-volume-ids"

// ExtVolumeAttachNotStarted is set to "true" by Cubelet only for failed
// Create responses that never attempted to attach a plugin volume.
const ExtVolumeAttachNotStarted = "cube-volume-attach-not-started"

// ExtVolumeCreateRolledBack confirms that failed Create released all of its
// plugin volume holds. The same response can carry node last-detach events.
const ExtVolumeCreateRolledBack = "cube-volume-create-rolled-back"
