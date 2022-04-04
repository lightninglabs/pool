module github.com/lightninglabs/pool

go 1.16

require (
	github.com/btcsuite/btcd v0.22.0-beta.0.20220316175102-8d5c75c28923
	github.com/btcsuite/btcd/btcec/v2 v2.1.3
	github.com/btcsuite/btcd/btcutil v1.1.1
	github.com/btcsuite/btcd/chaincfg/chainhash v1.0.1
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet v0.14.1-0.20220322182735-b0001c262734
	github.com/btcsuite/btcwallet/wallet/txrules v1.2.0
	github.com/btcsuite/btcwallet/wtxmgr v1.5.0
	github.com/davecgh/go-spew v1.1.1
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1
	github.com/golang/mock v1.6.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.17-beta.0.20220328072456-4a2632d0be38
	github.com/lightninglabs/lndclient v1.0.1-0.20220328143152-e7bebec99366
	github.com/lightninglabs/pool/auctioneerrpc v1.0.5
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.14.1-beta.0.20220330161355-8072b20d548d
	github.com/lightningnetwork/lnd/cert v1.1.1
	github.com/lightningnetwork/lnd/tlv v1.0.2
	github.com/lightningnetwork/lnd/tor v1.0.0
	github.com/stretchr/testify v1.7.0
	github.com/urfave/cli v1.22.4
	go.etcd.io/bbolt v1.3.6
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	google.golang.org/grpc v1.39.0
	google.golang.org/protobuf v1.27.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

replace github.com/lightninglabs/pool/auctioneerrpc => ./auctioneerrpc
