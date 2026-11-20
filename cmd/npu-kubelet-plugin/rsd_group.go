package main

import (
	"context"

	host "github.com/rbln-sw/rblnlib-go/pkg/host/v1"
	smi "github.com/rbln-sw/rblnlib-go/pkg/smi/v1"
)

func prepareRSDGroup(ctx context.Context, busIDs []string) (string, error) {
	c, err := smi.New(smi.Options{ReplaceIntersectingGroups: true})
	if err != nil {
		return "", err
	}
	group, err := c.RecreateGroup(ctx, busIDs)
	if err != nil {
		return "", err
	}
	machine, err := host.New(host.Options{})
	if err != nil {
		return "", err
	}
	node, err := machine.WaitForNode(ctx, group.Node)
	return node.HostPath, err
}
