module github.com/lightninglabs/pool

go 1.14

require (
	github.com/btcsuite/btcd v0.21.0-beta.0.20210513141527-ee5896bad5be
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v1.0.3-0.20210527170813-e2ba6805a890
	github.com/btcsuite/btcwallet v0.12.1-0.20210519225359-6ab9b615576f
	github.com/btcsuite/btcwallet/wallet/txrules v1.0.0
	github.com/btcsuite/btcwallet/wtxmgr v1.3.1-0.20210706234807-aaf03fee735a
	github.com/davecgh/go-spew v1.1.1
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.12.0-8
	github.com/lightninglabs/pool/auctioneerrpc v1.0.2
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.13.0-beta.rc5.0.20210728112744-ebabda671786
	github.com/lightningnetwork/lnd/cert v1.0.3
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

replace github.com/lightningnetwork/lnd => github.com/wpaulino/lnd v0.2.1-alpha.0.20210728230947-297dc5461e32

replace github.com/lightninglabs/lndclient => github.com/wpaulino/lndclient v1.0.1-0.20210728232320-919d63f1f469
