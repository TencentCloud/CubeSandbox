package cubebox

import (
	"errors"
	"fmt"
	"log"

	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/urfave/cli"
)

var DestroyCommand = cli.Command{
	Name:      "destroy",
	Aliases:   []string{"rm"},
	Usage:     "destroy sandbox instances",
	ArgsUsage: "<sandbox-id> [sandbox-id ...]",
	Action: func(c *cli.Context) error {
		if c.NArg() == 0 {
			_ = cli.ShowCommandHelp(c, "destroy")
			return errors.New("sandbox id is required")
		}

		serverList = getServerAddrs(c)
		if len(serverList) == 0 {
			log.Printf("no server addr")
			return errors.New("no server addr")
		}
		port = c.GlobalString("port")

		var rmErr error
		for _, sandboxID := range c.Args() {
			err := doInnerDestroySandbox(c, sandboxID, map[string]string{constants.Caller: "mastercli"}, "")
			if err != nil {
				log.Printf("destroy failed: %s %s\n", sandboxID, err.Error())
				rmErr = errors.Join(rmErr, fmt.Errorf("%s: %w", sandboxID, err))
				continue
			}
			log.Printf("destroyed: %s\n", sandboxID)
		}

		return rmErr
	},
}
