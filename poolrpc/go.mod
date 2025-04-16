module github.com/lightninglabs/pool/poolrpc

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0
	github.com/lightninglabs/pool/auctioneerrpc v1.1.2
	google.golang.org/grpc v1.65.0
	google.golang.org/protobuf v1.34.2
)

require (
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240814211410-ddb44dafa142 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 // indirect
)

replace github.com/lightninglabs/pool/auctioneerrpc => ../auctioneerrpc

replace google.golang.org/protobuf => github.com/lightninglabs/protobuf-go-hex-display v1.34.2-hex-display

go 1.23.6
