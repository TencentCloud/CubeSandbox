// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package nodestatus

import (
	"context"
	"fmt"
	"net"
	goruntime "runtime"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	cloudproviderapi "k8s.io/cloud-provider/api"
	netutils "k8s.io/utils/net"

	imagestore "github.com/tencentcloud/CubeSandbox/Cubelet/internal/cube/store/image"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/controller/runtemplate/templatetypes"
	cubeletnodemeta "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/cubelet/nodemeta"
)

const (
	MaxNamesPerImageInNodeStatus = 5
)

type Setter func(ctx context.Context, node *cubeletnodemeta.Node) error

func NodeAddress(nodeIPs []net.IP,
	hostname string,
	externalCloudProvider bool,
) Setter {
	var nodeIP, secondaryNodeIP net.IP
	if len(nodeIPs) > 0 {
		nodeIP = nodeIPs[0]
	}
	preferIPv4 := nodeIP == nil || nodeIP.To4() != nil
	isPreferredIPFamily := func(ip net.IP) bool { return (ip.To4() != nil) == preferIPv4 }
	nodeIPSpecified := nodeIP != nil && !nodeIP.IsUnspecified()

	if len(nodeIPs) > 1 {
		secondaryNodeIP = nodeIPs[1]
	}
	secondaryNodeIPSpecified := secondaryNodeIP != nil && !secondaryNodeIP.IsUnspecified()

	return func(ctx context.Context, node *cubeletnodemeta.Node) error {
		if externalCloudProvider && nodeIPSpecified {

			if node.ObjectMeta.Annotations == nil {
				node.ObjectMeta.Annotations = make(map[string]string)
			}
			annotation := nodeIP.String()
			if secondaryNodeIPSpecified {
				annotation += "," + secondaryNodeIP.String()
			}
			node.ObjectMeta.Annotations[cloudproviderapi.AnnotationAlphaProvidedIPAddr] = annotation
		} else if node.ObjectMeta.Annotations != nil {

			delete(node.ObjectMeta.Annotations, cloudproviderapi.AnnotationAlphaProvidedIPAddr)
		}

		if externalCloudProvider {

			if len(node.Status.Addresses) > 0 {
				return nil
			}

			if nodeIP == nil {
				node.Status.Addresses = []corev1.NodeAddress{
					{Type: corev1.NodeHostName, Address: hostname},
				}
				return nil
			}
		}
		if nodeIPSpecified && secondaryNodeIPSpecified {
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: nodeIP.String()},
				{Type: corev1.NodeInternalIP, Address: secondaryNodeIP.String()},
				{Type: corev1.NodeHostName, Address: hostname},
			}
		} else {
			var ipAddr net.IP
			var err error

			if nodeIPSpecified {
				ipAddr = nodeIP
			} else if addr := netutils.ParseIPSloppy(hostname); addr != nil {
				ipAddr = addr
			} else {
				var addrs []net.IP
				addrs, _ = net.LookupIP(node.Name)
				for _, addr := range addrs {
					if isPreferredIPFamily(addr) {
						ipAddr = addr
						break
					} else if ipAddr == nil {
						ipAddr = addr
					}
				}
			}

			if ipAddr == nil {

				return fmt.Errorf("can't get ip address of node %s. error: %v", node.Name, err)
			}
			node.Status.Addresses = []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ipAddr.String()},
				{Type: corev1.NodeHostName, Address: hostname},
			}
		}
		return nil
	}
}

func GoRuntime() Setter {
	return func(ctx context.Context, node *cubeletnodemeta.Node) error {
		node.Status.NodeInfo.OperatingSystem = goruntime.GOOS
		node.Status.NodeInfo.Architecture = goruntime.GOARCH
		return nil
	}
}

func Images(nodeStatusMaxImages int32,
	imageListFunc func(ctx context.Context) ([]imagestore.Image, error),
) Setter {
	return func(ctx context.Context, node *cubeletnodemeta.Node) error {

		var imagesOnNode []cubeletnodemeta.ContainerImage

		containerImages, err := imageListFunc(ctx)
		if err != nil {
			node.Status.CubeImages = imagesOnNode
			return fmt.Errorf("error getting image list: %v", err)
		}

		if int(nodeStatusMaxImages) > -1 &&
			int(nodeStatusMaxImages) < len(containerImages) {
			containerImages = containerImages[0:nodeStatusMaxImages]
		}

		for _, image := range containerImages {

			names := sets.New[string](image.ID)
			names.Insert(image.References...)

			v := names.UnsortedList()
			if len(v) > MaxNamesPerImageInNodeStatus {
				v = v[0:MaxNamesPerImageInNodeStatus]
			}

			sort.Strings(v)
			imagesOnNode = append(imagesOnNode, cubeletnodemeta.ContainerImage{
				Names:     v,
				SizeBytes: image.Size,
				Namespace: image.Namespace,
				MediaType: string(image.MediaType),
			})
		}

		slices.SortFunc(imagesOnNode, func(a, b cubeletnodemeta.ContainerImage) int {
			if len(a.Names) == 0 || len(b.Names) == 0 {
				return 0
			}
			if a.Names[0] > b.Names[0] {
				return 1
			} else if a.Names[0] < b.Names[0] {
				return -1
			}
			return 0
		})

		node.Status.CubeImages = imagesOnNode
		return nil
	}
}

func LocalTemplate(localTemplateListFunc func(context.Context) (map[string]*templatetypes.LocalRunTemplate, error)) Setter {
	return func(ctx context.Context, node *cubeletnodemeta.Node) error {
		var templates []cubeletnodemeta.LocalTemplate

		localTemplates, err := localTemplateListFunc(ctx)
		if err != nil {
			return fmt.Errorf("failed to list local templates: %v", err)
		}
		for _, lt := range localTemplates {
			coreTemplate := cubeletnodemeta.LocalTemplate{
				TemplateID: lt.TemplateID,
				ID:         lt.Snapshot.Snapshot.ID,
				Media:      lt.Snapshot.Snapshot.Media,
				Path:       lt.Snapshot.Snapshot.Path,
				Namespace:  lt.Namespace,
			}
			templates = append(templates, coreTemplate)
		}
		node.Status.CubeTemplates = templates

		return nil
	}
}
