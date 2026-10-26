// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
)

type UserData struct {
	DataBytes []byte
	K8sPod    *corev1.Pod `json:"k8s_pod,omitempty"`
}

func ParseUserData(userData string) (*UserData, error) {
	if userData == "" {
		return nil, nil
	}

	var (
		data = &UserData{}
		err  error
	)

	data.DataBytes, err = base64.StdEncoding.DecodeString(userData)
	if err != nil {
		return nil, err
	}
	if len(data.DataBytes) == 0 {
		return nil, errors.New("userdata is empty")
	}
	return data, nil
}

func (ud *UserData) ParseK8sPod() error {
	if ud.K8sPod != nil {
		return nil
	}
	var err error
	for _, v := range strings.Split(string(ud.DataBytes), "\n") {
		v = strings.TrimSpace(v)
		kv := strings.SplitN(v, "=", 2)
		if len(kv) != 2 {
			continue
		}

		if kv[0] == "POD" {
			var podyaml []byte
			podyaml, err = base64.StdEncoding.DecodeString(strings.TrimSpace(kv[1]))
			if err != nil {
				return fmt.Errorf("decode base64 pod of userdata error: %v", err)
			}

			if len(podyaml) == 0 {
				return errors.New("pod yaml is empty")
			}
			decode := serializer.NewCodecFactory(scheme.Scheme).UniversalDeserializer().Decode

			obj, _, err := decode(podyaml, nil, nil)
			if err != nil {
				return fmt.Errorf("decode pod of userdata error: %v", err)
			}
			ud.K8sPod = obj.(*corev1.Pod)
		}
	}
	return nil
}
