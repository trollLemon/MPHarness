package lxd

import (
	"context"

	lxd "github.com/canonical/lxd/client"
	"github.com/canonical/lxd/shared/api"
)

var GuestUserSetup = guestUserSetup

var ReadyTimeout = &readyTimeout
var ReadyPollInterval = &readyPollInterval

type CommandRecord = commandRecord

func (c *Client) SetConnectImage(fn func(url string, args *lxd.ConnectionArgs) (lxd.ImageServer, error)) {
	c.connectImage = fn
}

func (c *Client) ResolveImage(image string) (lxd.ImageServer, api.Image, error) {
	return c.resolveImage(image)
}

func (c *Client) WaitForVMReady(ctx context.Context, name string) error {
	return c.waitForVMReady(ctx, name)
}
