// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package opts

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	osinterface "github.com/containerd/containerd/v2/pkg/os"
)

func TestWithDevices(t *testing.T) {
	testcases := []struct {
		name    string
		devices []*runtime.Device
		isLCOW  bool

		expectError            bool
		expectedWindowsDevices []specs.WindowsDevice
	}{
		{
			name: "empty",

			expectError: false,
		},

		{
			name:    "empty fields",
			devices: []*runtime.Device{{}},

			expectError: true,
		},
		{
			name:    "containerPath",
			devices: []*runtime.Device{{ContainerPath: "something"}},

			expectError: true,
		},
		{
			name:    "permissions",
			devices: []*runtime.Device{{Permissions: "something"}},

			expectError: true,
		},

		{
			name:    "hostPath_docker_style",
			devices: []*runtime.Device{{HostPath: "class/5B45201D-F2F2-4F3B-85BB-30FF1F953599"}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: "5B45201D-F2F2-4F3B-85BB-30FF1F953599", IDType: "class"}},
		},

		{
			name:    "hostPath_docker_style_non-class_idtype",
			devices: []*runtime.Device{{HostPath: "vpci-location-path/5B45201D-F2F2-4F3B-85BB-30FF1F953599"}},

			expectError: true,
		},

		{
			name: "hostPath_hcsshim_lcow_gpu",

			devices: []*runtime.Device{{HostPath: `gpu://PCIP\VEN_8086&DEV_43A2&SUBSYS_72708086&REV_00\3&11583659&0&F5`}},
			isLCOW:  true,

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: `PCIP\VEN_8086&DEV_43A2&SUBSYS_72708086&REV_00\3&11583659&0&F5`, IDType: "gpu"}},
		},
		{
			name:    "hostPath_hcsshim_wcow_location_path",
			devices: []*runtime.Device{{HostPath: "vpci-location-path://PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0001)"}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: "PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0001)", IDType: "vpci-location-path"}},
		},
		{
			name:    "hostPath_hcsshim_wcow_class_guid",
			devices: []*runtime.Device{{HostPath: "class://5B45201D-F2F2-4F3B-85BB-30FF1F953599"}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: "5B45201D-F2F2-4F3B-85BB-30FF1F953599", IDType: "class"}},
		},
		{
			name: "hostPath_hcsshim_wcow_gpu_hyper-v",

			devices: []*runtime.Device{{HostPath: `vpci://PCIP\VEN_8086&DEV_43A2&SUBSYS_72708086&REV_00\3&11583659&0&F5`}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: `PCIP\VEN_8086&DEV_43A2&SUBSYS_72708086&REV_00\3&11583659&0&F5`, IDType: "vpci"}},
		},

		{
			name:    "hostPath_hcsshim_lcow_sandbox_device",
			devices: []*runtime.Device{{HostPath: "/dev/fuse"}},
			isLCOW:  true,

			expectError: true,
		},

		{
			name:    "hostPath_no_slash",
			devices: []*runtime.Device{{HostPath: "no_slash"}},

			expectError: true,
		},
		{
			name:    "hostPath_but_no_type",
			devices: []*runtime.Device{{HostPath: "://5B45201D-F2F2-4F3B-85BB-30FF1F953599"}},

			expectError: true,
		},
		{
			name:    "hostPath_but_no_id",
			devices: []*runtime.Device{{HostPath: "gpu://"}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: "", IDType: "gpu"}},
		},
		{
			name:    "hostPath_dockerstyle_with_slashes_in_id",
			devices: []*runtime.Device{{HostPath: "class/slashed/id"}},

			expectError:            false,
			expectedWindowsDevices: []specs.WindowsDevice{{ID: "slashed/id", IDType: "class"}},
		},
		{
			name:    "hostPath_docker_style_non-class_idtypewith_slashes_in_id",
			devices: []*runtime.Device{{HostPath: "vpci-location-path/slashed/id"}},

			expectError: true,
		},
		{
			name: "hostPath_hcsshim_wcow_location_path_twice",
			devices: []*runtime.Device{
				{HostPath: "vpci-location-path://PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0001)"},
				{HostPath: "vpci-location-path://PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0002)"}},

			expectError: false,
			expectedWindowsDevices: []specs.WindowsDevice{
				{ID: "PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0001)", IDType: "vpci-location-path"},
				{ID: "PCIROOT(0)#PCI(0100)#PCI(0000)#PCI(0000)#PCI(0002)", IDType: "vpci-location-path"},
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				ctx = namespaces.WithNamespace(context.Background(), "testing")
				c   = &containers.Container{ID: t.Name()}
			)

			config := runtime.ContainerConfig{}
			config.Devices = tc.devices

			specOpts := []oci.SpecOpts{WithWindowsDevices(&config)}

			platform := "windows"
			if tc.isLCOW {
				platform = "linux"
			}

			spec, err := oci.GenerateSpecWithPlatform(ctx, nil, platform, c, specOpts...)
			if tc.expectError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tc.isLCOW {
				assert.NotNil(t, spec.Linux)
			} else {
				assert.Nil(t, spec.Linux)
			}

			if len(tc.expectedWindowsDevices) != 0 {
				require.NotNil(t, spec.Windows)
				require.NotNil(t, spec.Windows.Devices)
				assert.Equal(t, spec.Windows.Devices, tc.expectedWindowsDevices)
			} else if spec.Windows != nil && spec.Windows.Devices != nil {
				assert.Empty(t, spec.Windows.Devices)
			}

			if spec.Linux != nil && spec.Linux.Devices != nil {
				assert.Empty(t, spec.Linux.Devices)
			}
		})
	}
}

func TestDriveMounts(t *testing.T) {
	tests := []struct {
		mnt                   *runtime.Mount
		expectedContainerPath string
		expectedError         error
	}{
		{&runtime.Mount{HostPath: `C:\`, ContainerPath: `D:\foo`}, `D:\foo`, nil},
		{&runtime.Mount{HostPath: `C:\`, ContainerPath: `D:\`}, `D:\`, nil},
		{&runtime.Mount{HostPath: `C:\`, ContainerPath: `D:`}, `D:`, nil},
		{&runtime.Mount{HostPath: `\\.\pipe\a_fake_pipe_name_that_shouldnt_exist`, ContainerPath: `\\.\pipe\foo`}, `\\.\pipe\foo`, nil},

		{&runtime.Mount{HostPath: `C:\`, ContainerPath: `C:\`}, `C:\`, nil},

		{&runtime.Mount{HostPath: `C:\`, ContainerPath: `C:`}, ``, fmt.Errorf("destination path can not be C drive")},
	}
	var realOS osinterface.RealOS
	for _, test := range tests {
		parsedMount, err := parseMount(realOS, test.mnt)
		if err != nil && !strings.EqualFold(err.Error(), test.expectedError.Error()) {
			t.Fatalf("expected err: %s, got %s instead", test.expectedError, err)
		} else if err == nil && test.expectedContainerPath != parsedMount.Destination {
			t.Fatalf("expected container path: %s, got %s instead", test.expectedContainerPath, parsedMount.Destination)
		}
	}
}
