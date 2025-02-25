package commands

import (
	"context"
	"errors"
	"strings"
	"time"

	core "github.com/ipsn/go-ipfs/core"
	coreapi "github.com/ipsn/go-ipfs/core/coreapi"
	coreiface "github.com/ipsn/go-ipfs/core/coreapi/interface"
	loader "github.com/ipsn/go-ipfs/plugin/loader"

	"github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-ipfs-cmds"
	config "github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-ipfs-config"
	logging "github.com/ipsn/go-ipfs/gxlibs/github.com/ipfs/go-log"
)

var log = logging.Logger("command")

// Context represents request context
type Context struct {
	Online     bool
	ConfigRoot string
	ReqLog     *ReqLog

	Plugins *loader.PluginLoader

	config     *config.Config
	LoadConfig func(path string) (*config.Config, error)

	api           coreiface.CoreAPI
	node          *core.IpfsNode
	ConstructNode func() (*core.IpfsNode, error)
}

// GetConfig returns the config of the current Command execution
// context. It may load it with the provided function.
func (c *Context) GetConfig() (*config.Config, error) {
	var err error
	if c.config == nil {
		if c.LoadConfig == nil {
			return nil, errors.New("nil LoadConfig function")
		}
		c.config, err = c.LoadConfig(c.ConfigRoot)
	}
	return c.config, err
}

// GetNode returns the node of the current Command execution
// context. It may construct it with the provided function.
func (c *Context) GetNode() (*core.IpfsNode, error) {
	var err error
	if c.node == nil {
		if c.ConstructNode == nil {
			return nil, errors.New("nil ConstructNode function")
		}
		c.node, err = c.ConstructNode()
	}
	return c.node, err
}

// GetAPI returns CoreAPI instance backed by ipfs node.
// It may construct the node with the provided function
func (c *Context) GetAPI() (coreiface.CoreAPI, error) {
	if c.api == nil {
		n, err := c.GetNode()
		if err != nil {
			return nil, err
		}
		c.api, err = coreapi.NewCoreAPI(n)
		if err != nil {
			return nil, err
		}
	}
	return c.api, nil
}

// Context returns the node's context.
func (c *Context) Context() context.Context {
	n, err := c.GetNode()
	if err != nil {
		log.Debug("error getting node: ", err)
		return context.Background()
	}

	return n.Context()
}

// LogRequest adds the passed request to the request log and
// returns a function that should be called when the request
// lifetime is over.
func (c *Context) LogRequest(req *cmds.Request) func() {
	rle := &ReqLogEntry{
		StartTime: time.Now(),
		Active:    true,
		Command:   strings.Join(req.Path, "/"),
		Options:   req.Options,
		Args:      req.Arguments,
		ID:        c.ReqLog.nextID,
		log:       c.ReqLog,
	}
	c.ReqLog.AddEntry(rle)

	return func() {
		c.ReqLog.Finish(rle)
	}
}

// Close cleans up the application state.
func (c *Context) Close() {
	// let's not forget teardown. If a node was initialized, we must close it.
	// Note that this means the underlying req.Context().Node variable is exposed.
	// this is gross, and should be changed when we extract out the exec Context.
	if c.node != nil {
		log.Info("Shutting down node...")
		c.node.Close()
	}
}
