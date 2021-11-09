package test

import "github.com/lightninglabs/lndclient"

type SignerClient interface {
	lndclient.SignerClient
}

type WalletKitClient interface {
	lndclient.WalletKitClient
}
