package routing

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	sunbeampb "github.com/goevery/sunstone/internal/gen/sunbeam/v1"
	"github.com/goevery/sunstone/internal/gen/sunbeam/v1/sunbeampbconnect"
	"google.golang.org/protobuf/types/known/emptypb"
)

// controlHandler translates ConnectRPC route-control requests into Router
// calls, converting routes and errors between the wire and local types.
type controlHandler struct {
	sunbeampbconnect.UnimplementedSunbeamHandler
	router *Router
}

// newControlHandler adapts the router to the ConnectRPC Sunbeam service.
func newControlHandler(router *Router) *controlHandler {
	return &controlHandler{router: router}
}

// GetRoute gets the configured route.
func (h *controlHandler) GetRoute(_ context.Context, request *connect.Request[sunbeampb.GetRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	route, err := h.router.GetRoute(request.Msg.GetName())
	if err != nil {
		return nil, connectError(err)
	}

	return connect.NewResponse(routeToProto(route)), nil
}

// ListRoutes lists the configured route.
func (h *controlHandler) ListRoutes(_ context.Context, request *connect.Request[sunbeampb.ListRoutesRequest]) (*connect.Response[sunbeampb.ListRoutesResponse], error) {
	routes, err := h.router.ListRoutes(request.Msg.GetPageSize(), request.Msg.GetPageToken())
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
func (h *controlHandler) UpdateRoute(ctx context.Context, request *connect.Request[sunbeampb.UpdateRouteRequest]) (*connect.Response[sunbeampb.Route], error) {
	route, err := h.router.UpdateRoute(
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

// DeleteRoute deletes the configured route after draining admitted
// requests.
func (h *controlHandler) DeleteRoute(_ context.Context, request *connect.Request[sunbeampb.DeleteRouteRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := h.router.DeleteRoute(request.Msg.GetName(), request.Msg.GetAllowMissing()); err != nil {
		return nil, connectError(err)
	}

	return connect.NewResponse(&emptypb.Empty{}), nil
}

func routeFromProto(route *sunbeampb.Route) Route {
	if route == nil {
		return Route{}
	}

	return Route{
		Name: route.GetName(),
		Backend: Backend{
			Address:          route.GetBackend().GetAddress(),
			ContainerID:      route.GetBackend().GetContainerId(),
			StartupProbePath: route.GetBackend().GetStartupProbePath(),
		},
	}
}

func routeToProto(route Route) *sunbeampb.Route {
	return &sunbeampb.Route{
		Name: route.Name,
		Backend: &sunbeampb.Backend{
			Address:          route.Backend.Address,
			ContainerId:      route.Backend.ContainerID,
			StartupProbePath: route.Backend.StartupProbePath,
		},
	}
}

// connectError maps route-control failures onto wire codes.
func connectError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrFailedPrecondition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	// The pre-switch readiness rejection proves no route change was
	// applied, so callers may safely clean up after it.
	case errors.Is(err, ErrBackendNotReady):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
