package host

import (
	"context"
	"net"
)

// Session is the borrowed transport to one target VM: channels to remote
// Unix or TCP sockets over an established connection. The owner that
// established the connection owns its teardown; the runtime never closes
// it and never re-establishes it.
type Session interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}
