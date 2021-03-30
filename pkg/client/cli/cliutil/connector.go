package cliutil

import (
	"context"
	"time"

	"google.golang.org/grpc"
	empty "google.golang.org/protobuf/types/known/emptypb"

	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
)

func IsConnectorRunning() bool {
	return client.SocketExists(client.ConnectorSocketName)
}

// WithConnector (1) ensures (TODO) that the connector is running, (2) establishes a connection to
// it, and (3) runs the given function with that connection.
//
// Nested calls to WithConnector will reuse the outer connection.
func WithConnector(ctx context.Context, fn func(context.Context, connector.ConnectorClient) error) error {
	type connectorConnCtxKey struct{}
	if untyped := ctx.Value(connectorConnCtxKey{}); untyped != nil {
		conn := untyped.(*grpc.ClientConn)
		connectorClient := connector.NewConnectorClient(conn)
		return fn(ctx, connectorClient)
	}

	if !client.SocketExists(client.ConnectorSocketName) {
		panic("not yet implemented -- keep using connectorState for now")
	}

	conn, err := client.DialSocket(ctx, client.ConnectorSocketName)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx = context.WithValue(ctx, connectorConnCtxKey{}, conn)
	connectorClient := connector.NewConnectorClient(conn)

	return fn(ctx, connectorClient)
}

func QuitConnector(ctx context.Context) error {
	if IsConnectorRunning() {
		err := WithConnector(ctx, func(ctx context.Context, connectorClient connector.ConnectorClient) error {
			_, err := connectorClient.Quit(ctx, &empty.Empty{})
			return err
		})
		if err == nil {
			err = client.WaitUntilSocketVanishes("connector", client.ConnectorSocketName, 5*time.Second)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
