module github.com/lightninglabs/pool/poolrpc

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.22.0
	github.com/lightninglabs/pool/auctioneerrpc v1.1.2
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/macaroon-bakery.v2 v2.3.0
)

require (
	github.com/go-macaroon-bakery/macaroonpb v1.0.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/rogpeppe/fastuuid v1.2.0 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect; indirect	google.golang.org/genproto/googleapis/api v0.0.0-20240814211410-ddb44dafa142 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260817212433-ac3dfec99bb1 // indirect
	gopkg.in/errgo.v1 v1.0.1 // indirect
	gopkg.in/macaroon.v2 v2.1.0 // indirect
)

replace github.com/lightninglabs/pool/auctioneerrpc => ../auctioneerrpc

replace google.golang.org/protobuf => github.com/lightninglabs/protobuf-go-hex-display v1.34.2-hex-display

go 1.27.3
