package container

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	engineapi "github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/events"
	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/log"
	"golang.org/x/net/context"
)

// containerController conducts remote operations for a container. All calls
// are mostly naked calls to the client API, seeded with information from
// containerConfig.
type containerAdapter struct {
	client    engineapi.APIClient
	container *containerConfig
}

func newContainerAdapter(client engineapi.APIClient, task *api.Task) (*containerAdapter, error) {
	ctnr, err := newContainerConfig(task)
	if err != nil {
		return nil, err
	}

	return &containerAdapter{
		client:    client,
		container: ctnr,
	}, nil
}

func noopPrivilegeFn() (string, error) { return "", nil }

func (c *containerAdapter) pullImage(ctx context.Context) error {
	rc, err := c.client.ImagePull(ctx, c.container.image(),
		types.ImagePullOptions{
			PrivilegeFunc: noopPrivilegeFn,
		})
	if err != nil {
		return err
	}

	dec := json.NewDecoder(rc)
	m := map[string]interface{}{}
	for {
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		// TOOD(stevvooe): Report this status somewhere.
		logrus.Debugln("pull progress", m)
	}
	// if the final stream object contained an error, return it
	if errMsg, ok := m["error"]; ok {
		return fmt.Errorf("%v", errMsg)
	}
	return nil
}

func (c *containerAdapter) createNetworks(ctx context.Context) error {
	for _, network := range c.container.networks() {
		opts, err := c.container.networkCreateOptions(network)
		if err != nil {
			return err
		}

		if _, err := c.client.NetworkCreate(ctx, network, opts); err != nil {
			if isNetworkExistError(err, network) {
				continue
			}

			return err
		}
	}

	return nil
}

func (c *containerAdapter) removeNetworks(ctx context.Context) error {
	for _, nid := range c.container.networks() {
		if err := c.client.NetworkRemove(ctx, nid); err != nil {
			if isActiveEndpointError(err) {
				continue
			}

			log.G(ctx).Errorf("network %s remove failed", nid)
			return err
		}
	}

	return nil
}

func isActiveEndpointError(err error) bool {
	// TODO(mrjana): There is no proper error code for network not
	// found error in engine-api. Resort to string matching until
	// engine-api is fixed.
	return strings.Contains(err.Error(), "has active endpoints")
}

func isNetworkExistError(err error, name string) bool {
	// TODO(mrjana): There is no proper error code for network not
	// found error in engine-api. Resort to string matching until
	// engine-api is fixed.
	return strings.Contains(err.Error(), fmt.Sprintf("network with name %s already exists", name))
}

func (c *containerAdapter) create(ctx context.Context) error {
	if _, err := c.client.ContainerCreate(ctx,
		c.container.config(),
		c.container.hostConfig(),
		c.container.networkingConfig(),
		c.container.name()); err != nil {
		return err
	}

	return nil
}

func (c *containerAdapter) start(ctx context.Context) error {
	return c.client.ContainerStart(ctx, c.container.name())
}

func (c *containerAdapter) inspect(ctx context.Context) (types.ContainerJSON, error) {
	return c.client.ContainerInspect(ctx, c.container.name())
}

// events issues a call to the events API and returns a channel with all
// events. The stream of events can be shutdown by cancelling the context.
//
// A chan struct{} is returned that will be closed if the event procressing
// fails and needs to be restarted.
func (c *containerAdapter) events(ctx context.Context) (<-chan events.Message, <-chan struct{}, error) {
	// TODO(stevvooe): Move this to a single, global event dispatch. For
	// now, we create a connection per container.
	var (
		eventsq = make(chan events.Message)
		closed  = make(chan struct{})
	)

	log.G(ctx).Debugf("waiting on events")
	// TODO(stevvooe): For long running tasks, it is likely that we will have
	// to restart this under failure.
	rc, err := c.client.Events(ctx, types.EventsOptions{
		Since:   "0",
		Filters: c.container.eventFilter(),
	})
	if err != nil {
		return nil, nil, err
	}

	go func(rc io.ReadCloser) {
		defer rc.Close()
		defer close(closed)

		select {
		case <-ctx.Done():
			// exit
			return
		default:
		}

		dec := json.NewDecoder(rc)

		for {
			var event events.Message
			if err := dec.Decode(&event); err != nil {
				// TODO(stevvooe): This error handling isn't quite right.
				if err == io.EOF {
					return
				}

				log.G(ctx).Errorf("error decoding event: %v", err)
				return
			}

			select {
			case eventsq <- event:
			case <-ctx.Done():
				return
			}
		}
	}(rc)

	return eventsq, closed, nil
}

func (c *containerAdapter) shutdown(ctx context.Context) error {
	timeout, err := resolveTimeout(ctx)
	if err != nil {
		return err
	}

	// TODO(stevvooe): Sending Stop isn't quite right. The timeout is actually
	// a grace period between SIGTERM and SIGKILL. We'll have to play with this
	// a little but to figure how much we defer to the engine.
	return c.client.ContainerStop(ctx, c.container.name(), timeout)
}

func (c *containerAdapter) terminate(ctx context.Context) error {
	return c.client.ContainerKill(ctx, c.container.name(), "")
}

func (c *containerAdapter) remove(ctx context.Context) error {
	return c.client.ContainerRemove(ctx, c.container.name(), types.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
}

// resolveTimeout calculates the timeout for second granularity timeout using
// the context's deadline.
func resolveTimeout(ctx context.Context) (int, error) {
	timeout := 10 // we need to figure out how to pick this value.
	if deadline, ok := ctx.Deadline(); ok {
		left := deadline.Sub(time.Now())

		if left <= 0 {
			<-ctx.Done()
			return 0, ctx.Err()
		}

		timeout = int(left.Seconds())
	}
	return timeout, nil
}