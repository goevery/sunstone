package connectrpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	routingfeature "github.com/goevery/sunstone/internal/modules/routing/internal/features/routing"
)

// Adapter exposes route control through ConnectRPC.
type Adapter struct {
	sunbeampbconnect.UnimplementedSunbeamHandler
	routing *routingfeature.Feature
}

// New constructs a ConnectRPC route-control adapter.
func New(routing *routingfeature.Feature) *Adapter {
	return &Adapter{routing: routing}
}

// GetRoute gets the configured route.
func (a *Adapter) GetRoute(_ context.Context, request *connect.Request[sunbeampb.GetRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	route, err := a.routing.GetRoute(request.Msg.GetName())
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(routeToProto(route)), nil
}

// ListRoutes lists the configured route.
func (a *Adapter) ListRoutes(_ context.Context, request *connect.Request[sunbeampb.ListRoutesRequest]) (*connect.Response[sunbeampb.ListRoutesResponse], error) {
	routes, err := a.routing.ListRoutes(request.Msg.GetPageSize(), request.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(err)
	}
	response := &sunbeampb.ListRoutesResponse{Routes: make([]*sunbeampb.Route, len(routes))}
	for index, route := range routes {
		response.Routes[index] = routeToProto(route)
	}
	return connect.NewResponse(response), nil
}

// UpdateRoute updates the configured route.
func (a *Adapter) UpdateRoute(ctx context.Context, request *connect.Request[sunbeampb.UpdateRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	route, err := a.routing.UpdateRoute(
		ctx,
		routeFromProto(request.Msg.GetRoute()),
		request.Msg.GetUpdateMask().GetPaths(),
		request.Msg.GetAllowMissing(),
	)
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(routeToProto(route)), nil
}

func routeFromProto(route *sunbeampb.Route) routingfeature.Route {
	if route == nil {
		return routingfeature.Route{}
	}
	return routingfeature.Route{
		Name: route.GetName(),
		Backend: routingfeature.Backend{
			Address:          route.GetBackend().GetAddress(),
			ContainerID:      route.GetBackend().GetContainerId(),
			StartupProbePath: route.GetBackend().GetStartupProbePath(),
		},
	}
}

func routeToProto(route routingfeature.Route) *sunbeampb.Route {
	return &sunbeampb.Route{
		Name: route.Name,
		Backend: &sunbeampb.Backend{
			Address:          route.Backend.Address,
			ContainerId:      route.Backend.ContainerID,
			StartupProbePath: route.Backend.StartupProbePath,
		},
	}
}

func connectError(err error) error {
	switch {
	case errors.Is(err, routingfeature.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, routingfeature.ErrFailedPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, routingfeature.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
