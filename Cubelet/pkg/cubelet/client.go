// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubelet

import (
	"fmt"

	"github.com/containerd/plugin"

	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/constants"
)

type services struct {
	cubeboxService cubebox.CubeboxMgrServer
	imageService   images.ImagesServer
}

type ServicesOpt func(c *services)

type clientOpts struct {
	services *services
}

type ClientOpt func(c *clientOpts) error

func WithServices(opts ...ServicesOpt) ClientOpt {
	return func(c *clientOpts) error {
		c.services = &services{}
		for _, o := range opts {
			o(c.services)
		}
		return nil
	}
}

func WithCubeboxClient(cubeboxService cubebox.CubeboxMgrServer) ServicesOpt {
	return func(s *services) {
		s.cubeboxService = cubeboxService
	}
}

func WithImageClient(imageService images.ImagesServer) ServicesOpt {
	return func(s *services) {
		s.imageService = imageService
	}
}

func WithInMemoryService(ic *plugin.InitContext) ([]ServicesOpt, error) {
	plugins, err := ic.GetByType(constants.CubeboxServicePlugin)
	if err != nil {
		return nil, fmt.Errorf("failed to get ServicePlugin plugin: %w", err)
	}

	opts := []ServicesOpt{}
	for s, fn := range map[string]func(interface{}) ServicesOpt{
		constants.CubeboxServiceID.ID(): func(s interface{}) ServicesOpt {
			return WithCubeboxClient(s.(cubebox.CubeboxMgrServer))
		},
	} {
		i := plugins[s]
		if i == nil {
			return nil, fmt.Errorf("instance of service %q not found", s)
		}
		opts = append(opts, fn(i))
	}
	return opts, nil
}

type Client struct {
	services
}

func New(address string, opts ...ClientOpt) (*Client, error) {
	var copts clientOpts
	for _, o := range opts {
		if err := o(&copts); err != nil {
			return nil, err
		}
	}

	c := &Client{}
	if copts.services != nil {
		c.services = *copts.services
	}
	return c, nil
}

func (c *Client) CubeBoxService() cubebox.CubeboxMgrServer {
	if c.cubeboxService != nil {
		return c.cubeboxService
	}
	return nil
}

func (c *Client) ImageService() images.ImagesServer {
	if c.imageService != nil {
		return c.imageService
	}
	return nil
}
