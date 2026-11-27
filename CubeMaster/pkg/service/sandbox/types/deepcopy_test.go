// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package types

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/pkgs/proto/services/images/v1"
)

func ptrTo[T any](value T) *T {
	return &value
}

// fullCreateRequest returns a request with every field set to a non-zero
// value, so a field the copy forgets is visible to the two tests below instead
// of hiding behind an omitted JSON key.
func fullCreateRequest() *CreateCubeSandboxReq {
	return &CreateCubeSandboxReq{
		Request:     &Request{RequestID: "req-1"},
		Timeout:     ptrTo(900),
		SnapshotDir: "/var/lib/cube/snapshot",
		InsId:       "ins-1",
		InsIp:       "10.0.0.1",
		Volumes: []*Volume{{
			Name: "vol1",
			VolumeSource: &VolumeSource{
				EmptyDir:    &EmptyDirVolumeSource{Medium: 1, SizeLimit: "1Gi"},
				SandboxPath: &SandboxPathVolumeSource{Path: "/sandbox", Type: "dir"},
				HostDirVolumeSources: &HostDirVolumeSources{
					VolumeSources: []*HostDirSource{{Name: "hd1", HostPath: "/host/data"}},
				},
				Image: &imagev1.ImageVolumeSource{
					Reference: &imagev1.ImageSpec{
						Image:        "harbor.local/vol:1.0",
						Annotations:  map[string]string{"cube.image.username": "user"},
						StorageMedia: "xfs",
						Namespace:    "default",
					},
					PullPolicy: imagev1.PullPolicy_Never,
				},
				PluginVolume: &PluginVolumeSource{
					Driver:  "nfs",
					Options: map[string]string{"volume_id": "v-1"},
				},
			},
		}},
		CubeNetworkConfig: &CubeNetworkConfig{
			AllowInternetAccess: ptrTo(true),
			AllowPublicTraffic:  ptrTo(false),
			AllowOut:            []string{"*.example.com"},
			DenyOut:             []string{"bad.example.com"},
			MaskRequestHost:     ptrTo("internal.example.com"),
			Rules: []*EgressRule{{
				Name: "rule1",
				Match: &EgressRuleMatch{
					SNI:    ptrTo("api.example.com"),
					Host:   ptrTo("api.example.com"),
					Method: []string{"GET", "POST"},
					Path:   ptrTo("/v1/*"),
					Scheme: ptrTo("https"),
					Port:   ptrTo(8443),
				},
				Action: &EgressRuleAction{
					Allow: true,
					Audit: ptrTo("on"),
					Inject: []*EgressRuleInject{{
						Header: "x-api-key",
						Secret: "s3cret",
						Format: ptrTo("${token}"),
					}},
				},
			}},
		},
		Containers: []*Container{{
			Name: "main",
			Image: &ImageSpec{
				Image:             "harbor.local/app:1.0",
				Name:              "registry-user",
				Token:             "registry-token",
				Annotations:       map[string]string{"cube.image.username": "registry-user"},
				StorageMedia:      "xfs",
				WritableLayerSize: "2Gi",
			},
			Command:    []string{"/bin/sh"},
			Args:       []string{"-c", "sleep 1"},
			WorkingDir: "/app",
			Envs:       []*KeyValue{{Key: "K", Value: "V"}},
			VolumeMounts: []*cubeboxv1.VolumeMounts{{
				Name:          "vol1",
				ContainerPath: "/data",
				Readonly:      true,
				Propagation:   cubeboxv1.MountPropagation_PROPAGATION_BIDIRECTIONAL,
				Uid:           "1000",
			}},
			RLimit: &RLimit{NoFile: 1024},
			Resources: &Resource{
				Cpu:   "2",
				Mem:   "2Gi",
				Limit: &RequestLimit{Cpu: "4", Mem: "4Gi"},
			},
			SecurityContext: &ContainerSecurityContext{
				Capabilities: &Capability{
					AddCapabilities:        []string{"SYS_ADMIN"},
					DropCapabilities:       []string{"NET_RAW"},
					AddAmbientCapabilities: []string{"NET_BIND_SERVICE"},
				},
				Privileged:     true,
				RunAsUser:      &Int64Value{Value: 1000},
				NoNewPrivs:     true,
				RunAsGroup:     &Int64Value{Value: 1000},
				RunAsUsername:  "app",
				ReadonlyRootfs: true,
			},
			Probe: &Probe{
				ProbeHandler: &ProbeHandler{
					TCPSocket: &TCPSocketAction{Port: 8080, Host: "127.0.0.1"},
					Ping:      &PingAction{Udp: true},
					HttpGet: &HTTPGetAction{
						Path: ptrTo("/healthz"),
						Port: 8080,
						Host: ptrTo("localhost"),
						HttpHeaders: []*HTTPHeader{{
							Name:  ptrTo("Authorization"),
							Value: ptrTo("Bearer x"),
						}},
					},
				},
				InitialDelaySeconds: 1,
				TimeoutSeconds:      2,
				InitialDelayMs:      3,
				TimeoutMs:           4,
				PeriodMs:            5,
				SuccessThreshold:    6,
				FailureThreshold:    7,
				ProbeTimeoutMs:      8,
			},
			LivenessProbe: &cubeboxv1.LivenessProbe{
				InitialDelaySecond: 30,
				PeriodSecond:       10,
				SuccessThreshold:   1,
				FailureThreshold:   3,
				ProbeTimeoutSecond: 1,
			},
			Sysctls: map[string]string{"net.ipv4.ip_forward": "1"},
			Syscalls: []*SysCall{{
				Names:  []string{"chmod"},
				Action: "SCMP_ACT_ALLOW",
				Errno:  1,
				Args: []*LinuxSeccompArg{{
					Index:    0,
					Value:    1,
					ValueTwo: 2,
					Op:       "SCMP_CMP_EQ",
				}},
			}},
			DnsConfig: &DNSConfig{
				Servers:  []string{"8.8.8.8"},
				Searches: []string{"svc.local"},
				Options:  []string{"ndots:5"},
			},
			Annotations: map[string]string{"cube.append": "x"},
			HostAliases: []*HostAlias{{Hostnames: []string{"db.local"}, Ip: "10.0.0.9"}},
			Prestop: &PreStop{
				TerminationGracePeriodMs: 1000,
				LifecyleHandler: &LifecycleHandler{
					HttpGet: &HTTPGetAction{
						Path: ptrTo("/prestop"),
						Port: 8081,
						Host: ptrTo("localhost"),
						HttpHeaders: []*HTTPHeader{{
							Name:  ptrTo("X-Pre"),
							Value: ptrTo("1"),
						}},
					},
				},
			},
			Poststop: &PostStop{
				TimeoutMs: 2000,
				LifecyleHandler: &LifecycleHandler{
					HttpGet: &HTTPGetAction{
						Path: ptrTo("/poststop"),
						Port: 8082,
						Host: ptrTo("localhost"),
						HttpHeaders: []*HTTPHeader{{
							Name:  ptrTo("X-Post"),
							Value: ptrTo("1"),
						}},
					},
				},
			},
			Id: "ctr-1",
			Hooks: &Hooks{Prestart: []*Hook{{
				Path:    "/hook",
				Args:    []string{"--flag"},
				Env:     []string{"E=1"},
				Timeout: ptrTo(int32(5)),
			}}},
		}},
		Annotations:       map[string]string{"cube.master.image.username": "registry-user"},
		Labels:            map[string]string{"app": "demo"},
		CreateTimeEnvVars: map[string]string{"E": "1"},
		DistributionScope: []string{"zone-a"},
		InstanceType:      "cubebox",
		NetworkType:       "vpc",
		Backend:           "xfs",
		SnapshotMode:      "full",
		RestartPolicy:     "OnFailure",
		RestartBackoff: &cubeboxv1.RestartBackoffConfig{
			InitialIntervalSecond: 1,
			MaxIntervalSecond:     60,
			Multiplier:            2,
			MaxRestarts:           5,
			Jitter:                0.1,
			StableDurationSecond:  30,
		},
		LivenessProbe: &cubeboxv1.LivenessProbe{
			InitialDelaySecond: 30,
			PeriodSecond:       10,
			SuccessThreshold:   1,
			FailureThreshold:   3,
			ProbeTimeoutSecond: 1,
		},
		RuntimeHandler: "runsc",
		Namespace:      "default",
		AutoPause:      true,
		AutoResume:     true,
	}
}

// minimalCreateRequest is shaped like a typical template definition: one
// container, an image, cpu/mem and the annotations a template carries.
func minimalCreateRequest() *CreateCubeSandboxReq {
	return &CreateCubeSandboxReq{
		Request: &Request{RequestID: "req-1"},
		Timeout: ptrTo(3600),
		Containers: []*Container{{
			Name:      "main",
			Image:     &ImageSpec{Image: "harbor.local/app:1.0"},
			Resources: &Resource{Cpu: "2", Mem: "2Gi"},
		}},
		Annotations: map[string]string{
			"app":   "demo",
			"owner": "perf",
		},
		DistributionScope: []string{"zone-a"},
		InstanceType:      "cubebox",
	}
}

func mustJSON(t *testing.T, req *CreateCubeSandboxReq) string {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(payload)
}

// TestCreateCubeSandboxReqDeepCopyMatchesJSONClone pins the semantics the clone
// has to keep: callers persist the copy (sandbox spec) and fingerprint it
// (template commit), so it must serialize exactly like the JSON round trip it
// replaced. A scalar field dropped from one of the explicit literal
// constructions in DeepCopy shows up here as a missing JSON key.
func TestCreateCubeSandboxReqDeepCopyMatchesJSONClone(t *testing.T) {
	req := fullCreateRequest()
	// The reference implementation, kept in the test so the contract stays
	// visible next to the copy it constrains.
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	viaJSON := &CreateCubeSandboxReq{}
	if err := json.Unmarshal(payload, viaJSON); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	want := mustJSON(t, viaJSON)
	got := mustJSON(t, req.DeepCopy())
	if got != want {
		t.Fatalf("deep copy does not serialize like the JSON clone\n got: %s\nwant: %s", got, want)
	}
}

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

// TestCreateCubeSandboxReqDeepCopySharesNoMutableState walks the original and
// the copy in lockstep and fails on any pointer, slice or map that still points
// at the original's backing memory. It inspects the structs by type instead of
// a hand-written list of fields, so a mutable field added later is covered
// without touching this test: the struct assignment at the top of DeepCopy
// copies it by reference, which is exactly what this test rejects.
func TestCreateCubeSandboxReqDeepCopySharesNoMutableState(t *testing.T) {
	req := fullCreateRequest()
	assertNoSharedState(t, "request", reflect.ValueOf(req), reflect.ValueOf(req.DeepCopy()))
}

func assertNoSharedState(t *testing.T, path string, orig, copied reflect.Value) {
	t.Helper()
	if orig.Kind() != copied.Kind() {
		t.Fatalf("%s: kind changed from %s to %s", path, orig.Kind(), copied.Kind())
	}
	switch orig.Kind() {
	case reflect.Pointer:
		if orig.IsNil() || copied.IsNil() {
			if orig.IsNil() != copied.IsNil() {
				t.Fatalf("%s: nil-ness differs (orig nil=%v, copy nil=%v)", path, orig.IsNil(), copied.IsNil())
			}
			return
		}
		if orig.Pointer() == copied.Pointer() {
			t.Fatalf("%s: copy aliases the original pointer", path)
		}
		// A cloned protobuf message owns its own graph; its internals are not
		// part of the copy contract here.
		if orig.Type().Implements(protoMessageType) {
			return
		}
		assertNoSharedState(t, path, orig.Elem(), copied.Elem())
	case reflect.Slice:
		if orig.IsNil() != copied.IsNil() {
			t.Fatalf("%s: nil-ness differs (orig nil=%v, copy nil=%v)", path, orig.IsNil(), copied.IsNil())
		}
		if orig.IsNil() {
			return
		}
		if orig.Len() != copied.Len() {
			t.Fatalf("%s: length changed from %d to %d", path, orig.Len(), copied.Len())
		}
		if orig.Len() > 0 && orig.Pointer() == copied.Pointer() {
			t.Fatalf("%s: copy shares the original backing array", path)
		}
		for i := 0; i < orig.Len(); i++ {
			assertNoSharedState(t, fmt.Sprintf("%s[%d]", path, i), orig.Index(i), copied.Index(i))
		}
	case reflect.Map:
		if orig.IsNil() != copied.IsNil() {
			t.Fatalf("%s: nil-ness differs (orig nil=%v, copy nil=%v)", path, orig.IsNil(), copied.IsNil())
		}
		if orig.IsNil() {
			return
		}
		if orig.Len() != copied.Len() {
			t.Fatalf("%s: length changed from %d to %d", path, orig.Len(), copied.Len())
		}
		if orig.UnsafePointer() == copied.UnsafePointer() {
			t.Fatalf("%s: copy shares the original map", path)
		}
		iter := orig.MapRange()
		for iter.Next() {
			value := copied.MapIndex(iter.Key())
			if !value.IsValid() {
				t.Fatalf("%s: key %v missing from the copy", path, iter.Key())
			}
			assertNoSharedState(t, fmt.Sprintf("%s[%v]", path, iter.Key()), iter.Value(), value)
		}
	case reflect.Interface:
		if orig.IsNil() != copied.IsNil() {
			t.Fatalf("%s: nil-ness differs", path)
		}
		if orig.IsNil() {
			return
		}
		assertNoSharedState(t, path, orig.Elem(), copied.Elem())
	case reflect.Struct:
		for i := 0; i < orig.NumField(); i++ {
			field := orig.Type().Field(i)
			if field.PkgPath != "" {
				// Unexported (protobuf bookkeeping): nothing to copy.
				continue
			}
			assertNoSharedState(t, path+"."+field.Name, orig.Field(i), copied.Field(i))
		}
	}
}

// TestCreateCubeSandboxReqDeepCopyNil documents the nil receiver, which the
// template cache relies on (a nil request is a miss, not a panic).
func TestCreateCubeSandboxReqDeepCopyNil(t *testing.T) {
	var req *CreateCubeSandboxReq
	if got := req.DeepCopy(); got != nil {
		t.Fatalf("nil request copied to %#v", got)
	}
}

func BenchmarkCreateCubeSandboxReqClone(b *testing.B) {
	cases := map[string]*CreateCubeSandboxReq{
		"full":    fullCreateRequest(),
		"minimal": minimalCreateRequest(),
	}
	for name, req := range cases {
		b.Run(name+"/json", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				payload, err := json.Marshal(req)
				if err != nil {
					b.Fatal(err)
				}
				out := &CreateCubeSandboxReq{}
				if err := json.Unmarshal(payload, out); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/deepcopy", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if out := req.DeepCopy(); out == nil {
					b.Fatal("nil copy")
				}
			}
		})
	}
}
