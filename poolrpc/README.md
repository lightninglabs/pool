# poolrpc

This poolrpc package implements both a client (trader) and server (auctioneer)
RPC system which is based off of the high-performance cross-platform
[gRPC](http://www.grpc.io/) RPC framework. By default, only the Go
client+server libraries are compiled within the package. In order to compile
the client side libraries for other supported languages, the `protoc` tool will
need to be used to generate the compiled protos for a specific language.

For a list of supported languages for `poolrpc`, see
[grpc.io/docs/languages](https://grpc.io/docs/languages/).

## Generate protobuf definitions

Make sure you have the exact versions of all libraries installed as described
in [the lnrpc/README.md of lnd](https://github.com/lightningnetwork/lnd/blob/master/lnrpc/README.md#generate-protobuf-definitions)
then run the `make rpc` command.
