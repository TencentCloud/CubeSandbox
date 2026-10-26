// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package runtemplate

import (
	"testing"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
)

func TestTemplateIdIndex(t *testing.T) {
	tests := []struct {
		name    string
		obj     interface{}
		want    []string
		wantErr bool
	}{
		{
			name: "valid template with template ID",
			obj: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "template-123",
				},
			},
			want:    []string{"template-123"},
			wantErr: false,
		},
		{
			name: "template with empty template ID",
			obj: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					TemplateID: "",
				},
			},
			want:    []string{""},
			wantErr: false,
		},
		{
			name:    "invalid object type",
			obj:     "not a template",
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := templateIdIndex(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Errorf("templateIdIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("templateIdIndex() = %v, want %v", got, tt.want)
					return
				}
				for i, v := range got {
					if v != tt.want[i] {
						t.Errorf("templateIdIndex() = %v, want %v", got, tt.want)
						break
					}
				}
			}
		})
	}
}

func TestImageIDIndex(t *testing.T) {
	tests := []struct {
		name    string
		obj     interface{}
		want    []string
		wantErr bool
	}{
		{
			name: "template with multiple images",
			obj: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					Namespace: "default",
				},
				Images: []templatetypes.LocalDistributionImage{
					{
						Image: imagestore.Image{
							ID: "image-1",
						},
					},
					{
						Image: imagestore.Image{
							ID: "image-2",
						},
					},
				},
			},
			want:    []string{"default/image-1", "default/image-2"},
			wantErr: false,
		},
		{
			name: "template with no images",
			obj: &templatetypes.LocalRunTemplate{
				DistributionReference: templatetypes.DistributionReference{
					Namespace: "default",
				},
				Images: []templatetypes.LocalDistributionImage{},
			},
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "invalid object type",
			obj:     123,
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := imageIDIndex(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Errorf("imageIDIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("imageIDIndex() returned %d items, want %d", len(got), len(tt.want))
					return
				}
				for i, v := range got {
					if v != tt.want[i] {
						t.Errorf("imageIDIndex()[%d] = %v, want %v", i, v, tt.want[i])
					}
				}
			}
		})
	}
}

func TestBaseBlockIDIndex(t *testing.T) {
	tests := []struct {
		name    string
		obj     interface{}
		want    []string
		wantErr bool
	}{
		{
			name: "template with volumes",
			obj: &templatetypes.LocalRunTemplate{
				Volumes: map[string]templatetypes.LocalBaseVolume{
					"vol-1": {
						Volume: templatetypes.VolumeSource{
							BaseBlockSource: templatetypes.BaseBlockVolumeSource{
								ID: "block-1",
							},
						},
					},
					"vol-2": {
						Volume: templatetypes.VolumeSource{
							BaseBlockSource: templatetypes.BaseBlockVolumeSource{
								ID: "block-2",
							},
						},
					},
				},
			},
			want:    nil,
			wantErr: false,
		},
		{
			name: "template with no volumes",
			obj: &templatetypes.LocalRunTemplate{
				Volumes: map[string]templatetypes.LocalBaseVolume{},
			},
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "invalid object type",
			obj:     []string{"invalid"},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := baseBlockIDIndex(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Errorf("baseBlockIDIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && tt.want != nil {
				if len(got) != len(tt.want) {
					t.Errorf("baseBlockIDIndex() returned %d items, want %d", len(got), len(tt.want))
				}
			}
		})
	}
}

func TestSnapshotIDIndex(t *testing.T) {
	tests := []struct {
		name    string
		obj     interface{}
		want    []string
		wantErr bool
	}{
		{
			name: "template with snapshot",
			obj: &templatetypes.LocalRunTemplate{
				Snapshot: templatetypes.LocalSnapshot{
					Snapshot: templatetypes.Snapshot{
						ID: "snapshot-123",
					},
				},
			},
			want:    []string{"snapshot-123"},
			wantErr: false,
		},
		{
			name: "template with empty snapshot name",
			obj: &templatetypes.LocalRunTemplate{
				Snapshot: templatetypes.LocalSnapshot{
					Snapshot: templatetypes.Snapshot{
						ID: "",
					},
				},
			},
			want:    []string{""},
			wantErr: false,
		},
		{
			name:    "invalid object type",
			obj:     map[string]string{"invalid": "type"},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotIDIndex(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Errorf("snapshotIDIndex() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("snapshotIDIndex() = %v, want %v", got, tt.want)
					return
				}
				for i, v := range got {
					if v != tt.want[i] {
						t.Errorf("snapshotIDIndex() = %v, want %v", got, tt.want)
						break
					}
				}
			}
		})
	}
}

func TestIndexerKeys(t *testing.T) {

	expectedKeys := []string{
		templateIDIndexerKey,
		imageNamespaceIDIndexerKey,
		snapshotIDIndexerKey,
	}

	if templateIDIndexerKey != "TemplateID" {
		t.Errorf("templateIDIndexerKey = %v, want 'TemplateID'", templateIDIndexerKey)
	}
	if imageNamespaceIDIndexerKey != "ImageNamespaceID" {
		t.Errorf("imageNamespaceIDIndexerKey = %v, want 'ImageNamespaceID'", imageNamespaceIDIndexerKey)
	}
	if snapshotIDIndexerKey != "BaseSnapshotID" {
		t.Errorf("snapshotIDIndexerKey = %v, want 'BaseSnapshotID'", snapshotIDIndexerKey)
	}

	for _, key := range expectedKeys {
		if _, ok := indexer[key]; !ok {
			t.Errorf("indexer missing key: %s", key)
		}
	}
}

func TestIndexerFunctions(t *testing.T) {
	template := &templatetypes.LocalRunTemplate{
		DistributionReference: templatetypes.DistributionReference{
			TemplateID:         "template-test",
			DistributionTaskID: "task-test",
			Namespace:          "test-ns",
		},
		Images: []templatetypes.LocalDistributionImage{
			{
				Image: imagestore.Image{
					ID: "image-test",
				},
			},
		},
		Volumes: map[string]templatetypes.LocalBaseVolume{
			"vol-1": {
				Volume: templatetypes.VolumeSource{
					BaseBlockSource: templatetypes.BaseBlockVolumeSource{
						ID: "block-test",
					},
				},
			},
		},
		Snapshot: templatetypes.LocalSnapshot{
			Snapshot: templatetypes.Snapshot{
				ID: "snapshot-test",
			},
		},
	}

	t.Run("all indexers work on same object", func(t *testing.T) {

		templateIDs, err := templateIdIndex(template)
		if err != nil {
			t.Errorf("templateIdIndex() error = %v", err)
		}
		if len(templateIDs) != 1 || templateIDs[0] != "template-test" {
			t.Errorf("templateIdIndex() = %v, want ['template-test']", templateIDs)
		}

		imageIDs, err := imageIDIndex(template)
		if err != nil {
			t.Errorf("imageIDIndex() error = %v", err)
		}
		if len(imageIDs) != 1 || imageIDs[0] != "test-ns/image-test" {
			t.Errorf("imageIDIndex() = %v, want ['test-ns/image-test']", imageIDs)
		}

		snapshotIDs, err := snapshotIDIndex(template)
		if err != nil {
			t.Errorf("snapshotIDIndex() error = %v", err)
		}
		if len(snapshotIDs) != 1 || snapshotIDs[0] != "snapshot-test" {
			t.Errorf("snapshotIDIndex() = %v, want ['snapshot-test']", snapshotIDs)
		}

		blockIDs, err := baseBlockIDIndex(template)
		if err != nil {
			t.Errorf("baseBlockIDIndex() error = %v", err)
		}

		expectedBlockID := "test-ns/block-test"
		if len(blockIDs) != 1 || blockIDs[0] != expectedBlockID {
			t.Errorf("baseBlockIDIndex() = %v, want ['%s']", blockIDs, expectedBlockID)
		}
	})
}
