module github.com/lightninglabs/pool

go 1.16

require (
	github.com/btcsuite/btcd v0.22.0-beta.0.20211005184431-e3449998be39
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v1.0.3-0.20210527170813-e2ba6805a890
	github.com/btcsuite/btcwallet v0.13.0
	github.com/btcsuite/btcwallet/wallet/txrules v1.1.0
	github.com/btcsuite/btcwallet/wtxmgr v1.3.1-0.20210822222949-9b5a201c344c
	github.com/davecgh/go-spew v1.1.1
	github.com/golang/mock v1.6.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.14.0-8
	github.com/lightninglabs/pool/auctioneerrpc v1.0.5
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.14.1-beta
	github.com/lightningnetwork/lnd/cert v1.1.0
	github.com/stretchr/testify v1.7.0
	github.com/urfave/cli v1.20.0
	go.etcd.io/bbolt v1.3.6
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	google.golang.org/grpc v1.38.0
	google.golang.org/protobuf v1.26.0
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

replace github.com/lightninglabs/pool/auctioneerrpc => ./auctioneerrpc
