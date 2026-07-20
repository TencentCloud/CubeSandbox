// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"errors"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/sandboxid"
)

func TestResolveSandboxIDFromListBySandboxPrefix(t *testing.T) {
	items := []*cubebox.CubeSandbox{
		{Id: "aabbccddeeff00112233445566778899"},
		{Id: "112233445566778899aabbccddeeff00"},
	}
	got, err := resolveSandboxIDFromList(items, "aabbccdd")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != items[0].Id {
		t.Fatalf("got=%q want=%q", got, items[0].Id)
	}
}

func TestResolveSandboxIDFromListByContainerPrefix(t *testing.T) {
	items := []*cubebox.CubeSandbox{
		{
			Id: "aabbccddeeff00112233445566778899",
			Containers: []*cubebox.Container{
				{Id: "contaabb00112233445566778899aabb"},
			},
		},
		{
			Id: "112233445566778899aabbccddeeff00",
			Containers: []*cubebox.Container{
				{Id: "cont112233445566778899aabbccddee"},
			},
		},
	}
	got, err := resolveSandboxIDFromList(items, "contaabb")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != items[0].Id {
		t.Fatalf("got=%q want sandbox id %q", got, items[0].Id)
	}
}

func TestResolveSandboxIDFromListContainerAmbiguous(t *testing.T) {
	items := []*cubebox.CubeSandbox{
		{
			Id:         "aabbccddeeff00112233445566778899",
			Containers: []*cubebox.Container{{Id: "contshared00112233445566778899aa"}},
		},
		{
			Id:         "112233445566778899aabbccddeeff00",
			Containers: []*cubebox.Container{{Id: "contshared00112233445566778899bb"}},
		},
	}
	_, err := resolveSandboxIDFromList(items, "contshared")
	if !errors.Is(err, sandboxid.ErrAmbiguous) {
		t.Fatalf("err=%v want ErrAmbiguous", err)
	}
}
