module github.com/lightninglabs/pool

go 1.14

require (
	github.com/btcsuite/btcd v0.22.0-beta.0.20210916191717-f8e6854197cd
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v1.0.3-0.20210527170813-e2ba6805a890
	github.com/btcsuite/btcwallet v0.12.1-0.20210826004415-4ef582f76b02
	github.com/btcsuite/btcwallet/wallet/txrules v1.1.0
	github.com/btcsuite/btcwallet/wtxmgr v1.3.1-0.20210822222949-9b5a201c344c
	github.com/davecgh/go-spew v1.1.1
	github.com/golang/protobuf v1.4.3
	github.com/grpc-ecosystem/grpc-gateway v1.14.6
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.12.0-8
	github.com/lightninglabs/pool/auctioneerrpc v1.0.2
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.13.3-beta
	github.com/lightningnetwork/lnd/cert v1.0.3
	github.com/stretchr/testify v1.7.0
	github.com/urfave/cli v1.20.0
	go.etcd.io/bbolt v1.3.5-0.20200615073812-232d8fc87f50
	golang.org/x/sync v0.0.0-20200317015054-43a5402ce75a
	google.golang.org/grpc v1.29.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

replace github.com/lightninglabs/pool/auctioneerrpc => ./auctioneerrpc

// Fix incompatibility of etcd go.mod package.
// See https://github.com/etcd-io/etcd/issues/11154
replace go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20201125193152-8a03d2e9614b
