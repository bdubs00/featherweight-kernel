package fw

// This file is the services: work that runs beside the agent for as long
// as the agent lives, such as the fleet of the workbench. A linked package
// registers a service in init, or a program passes one as an option. New
// starts the services after the kernel starts, and shutdown stops them
// first, before the decision points, the tools, the bus, and the ledger.

import (
	"context"
	"fmt"
	"io"
)

// A Service starts work beside an agent and returns what stops it, or nil
// when it has nothing to run for this agent, for example when its
// configuration block is absent. An error stops New.
type Service func(ctx context.Context, a *Agent) (io.Closer, error)

// startServices starts the registered services, unless WithoutDefaults
// left them out, and then the services of the options, unless
// WithoutServices left every service out.
func (a *Agent) startServices(ctx context.Context, o *options) error {
	if o.noServices {
		return nil
	}
	var services []Service
	if !o.noDefaults {
		services = RegisteredServices()
	}
	services = append(services, o.services...)
	for i, s := range services {
		c, err := s(ctx, a)
		if err != nil {
			a.closeServices()
			return fmt.Errorf("fw: service %d: %w", i+1, err)
		}
		if c != nil {
			a.services = append(a.services, c)
		}
	}
	return nil
}

// closeServices stops the services, the last one first.
func (a *Agent) closeServices() {
	for i := len(a.services) - 1; i >= 0; i-- {
		if err := a.services[i].Close(); err != nil {
			a.log.Warn("close service", "error", err)
		}
	}
	a.services = nil
}
