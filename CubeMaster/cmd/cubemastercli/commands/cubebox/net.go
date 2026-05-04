package cubebox

import (
	"net"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"github.com/urfave/cli"
)

const (
	keyAllowInternetAccess = "allow-internet-access"
	keyAllowOutCIDR        = "allow-out-cidr"
	keyDenyOutCIDR         = "deny-out-cidr"
)

func parseCubeVSParams(c *cli.Context) (*types.CubeVSContext, error) {
	if !c.IsSet(keyAllowInternetAccess) &&
		len(c.StringSlice(keyAllowOutCIDR)) == 0 &&
		len(c.StringSlice(keyDenyOutCIDR)) == 0 {
		return nil, nil
	}

	params := &types.CubeVSContext{}
	if c.IsSet(keyAllowInternetAccess) {
		val := c.Bool(keyAllowInternetAccess)
		params.AllowInternetAccess = &val
	}
	for _, v := range c.StringSlice(keyAllowOutCIDR) {
		_, _, err := net.ParseCIDR(v)
		if err != nil {
			return nil, err
		}
		params.AllowOut = append(params.AllowOut, v)
	}
	for _, v := range c.StringSlice(keyDenyOutCIDR) {
		_, _, err := net.ParseCIDR(v)
		if err != nil {
			return nil, err
		}
		params.DenyOut = append(params.DenyOut, v)
	}

	return params, nil
}

func mergeCubeVSParams(c *cli.Context, base *types.CubeVSContext) (*types.CubeVSContext, error) {
	if !c.IsSet(keyAllowInternetAccess) &&
		len(c.StringSlice(keyAllowOutCIDR)) == 0 &&
		len(c.StringSlice(keyDenyOutCIDR)) == 0 {
		return base, nil
	}

	if base == nil {
		base = &types.CubeVSContext{}
	}

	if c.IsSet(keyAllowInternetAccess) {
		val := c.Bool(keyAllowInternetAccess)
		base.AllowInternetAccess = &val
	}
	for _, v := range c.StringSlice(keyAllowOutCIDR) {
		_, _, err := net.ParseCIDR(v)
		if err != nil {
			return nil, err
		}
		base.AllowOut = append(base.AllowOut, v)
	}
	for _, v := range c.StringSlice(keyDenyOutCIDR) {
		_, _, err := net.ParseCIDR(v)
		if err != nil {
			return nil, err
		}
		base.DenyOut = append(base.DenyOut, v)
	}

	return base, nil
}
