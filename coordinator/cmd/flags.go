package main

import "github.com/urfave/cli/v2"

var (
	verifierFlag = cli.StringFlag{
		Name:  "verifier-socket-file",
		Usage: "The path of ipc-verifier socket file",
		Value: "/tmp/verifier.sock",
	}
	apiFlags = []cli.Flag{
		&wsPortFlag,
		&httpEnabledFlag,
		&httpListenAddrFlag,
		&httpPortFlag,
	}
	// wsPortFlag is websocket port
	wsPortFlag = cli.IntFlag{
		Name:  "ws.port",
		Usage: "WS-RPC server listening port",
		Value: 9000,
	}
	// httpEnabledFlag enable rpc server.
	httpEnabledFlag = cli.BoolFlag{
		Name:  "http",
		Usage: "Enable the HTTP-RPC server",
		Value: false,
	}
	// httpListenAddrFlag set the http address.
	httpListenAddrFlag = cli.StringFlag{
		Name:  "http.addr",
		Usage: "HTTP-RPC server listening interface",
		Value: "localhost",
	}
	// httpPortFlag set http.port.
	httpPortFlag = cli.IntFlag{
		Name:  "http.port",
		Usage: "HTTP-RPC server listening port",
		Value: 8390,
	}
)