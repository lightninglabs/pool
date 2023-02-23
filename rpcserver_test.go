package pool

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	gomock "github.com/golang/mock/gomock"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	ctxTimeout      = 1 * time.Second
	traderKeyStr    = "036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09"
	traderKeyRaw, _ = hex.DecodeString(traderKeyStr)
)

func getAccountKey(acctKey []byte) *btcec.PublicKey {
	res, _ := btcec.ParsePubKey(acctKey)
	return res
}

func genRenewAccountReq(accountKey []byte, relative uint32,
	feerate uint64, version uint32) *poolrpc.RenewAccountRequest {

	req := &poolrpc.RenewAccountRequest{
		AccountKey:      accountKey,
		FeeRateSatPerKw: feerate,
		NewVersion:      poolrpc.AccountVersion(version),
	}

	req.AccountKey = accountKey
	req.FeeRateSatPerKw = feerate
	req.AccountExpiry = &poolrpc.RenewAccountRequest_RelativeExpiry{
		RelativeExpiry: relative,
	}

	return req
}

var renewAccountTestCases = []struct {
	name          string
	lndVer        *verrpc.Version
	getReq        func() *poolrpc.RenewAccountRequest
	checkResponse func(*poolrpc.RenewAccountResponse) error
	mockSetter    func(*poolrpc.RenewAccountRequest,
		*account.MockManager, *MockMarshaler)
	expectedError string
}{{
	name: "we are able to successfully renew an old account using an old " +
		"lnd that does not support taproot accounts",
	lndVer: &verrpc.Version{AppMajor: 0,
		AppMinor: 14,
		AppPatch: 3,
	},
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 10, 1000, 1)
	},
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, marshalerMock *MockMarshaler) {
		// Renew account params
		bestHeight := uint32(100)
		feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
		expiryHeight := req.GetAbsoluteExpiry()
		if expiryHeight == 0 {
			expiryHeight = 100 + req.GetRelativeExpiry()
		}
		version := account.VersionInitialNoVersion
		acc := &account.Account{}
		tx := &wire.MsgTx{}
		accMgr.EXPECT().
			RenewAccount(
				gomock.Any(), getAccountKey(req.AccountKey),
				expiryHeight, feeRate, bestHeight, version,
			).
			Return(acc, tx, nil)

		rpcAccount := poolrpc.Account{}
		marshalerMock.EXPECT().
			MarshallAccountsWithAvailableBalance(
				gomock.Any(), gomock.Eq([]*account.Account{acc}),
			).
			Return([]*poolrpc.Account{&rpcAccount}, nil)
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
}, {
	name:   "we are able to successfully renew an account",
	lndVer: minimalCompatibleVersion,
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 10, 1000, 0)
	},
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, marshalerMock *MockMarshaler) {
		// Renew account params
		bestHeight := uint32(100)
		feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
		expiryHeight := req.GetAbsoluteExpiry()
		if expiryHeight == 0 {
			expiryHeight = 100 + req.GetRelativeExpiry()
		}
		version := account.VersionTaprootEnabled
		acc := &account.Account{}
		tx := &wire.MsgTx{}
		accMgr.EXPECT().
			RenewAccount(
				gomock.Any(), getAccountKey(req.AccountKey),
				expiryHeight, feeRate, bestHeight, version,
			).
			Return(acc, tx, nil)

		rpcAccount := poolrpc.Account{}
		marshalerMock.EXPECT().
			MarshallAccountsWithAvailableBalance(
				gomock.Any(), gomock.Eq([]*account.Account{acc}),
			).
			Return([]*poolrpc.Account{&rpcAccount}, nil)
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
}, {
	name:   "account key must be valid",
	lndVer: minimalCompatibleVersion,
	getReq: func() *poolrpc.RenewAccountRequest {
		return &poolrpc.RenewAccountRequest{
			AccountKey: []byte{3, 5, 8},
		}
	},
	expectedError: "malformed public key: invalid length: 3",
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, marshalerMock *MockMarshaler) {
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
}, {
	name:   "req should specify absolute/relative expiry",
	lndVer: minimalCompatibleVersion,
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 0, 1000, 0)
	},
	expectedError: "either relative or absolute height must be specified",
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, marshalerMock *MockMarshaler) {
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
}, {
	name:   "req should specify a valid fee rate",
	lndVer: minimalCompatibleVersion,
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 100, 0, 0)
	},
	expectedError: "fee rate of 0 sat/kw is too low, minimum is 253 sat/kw",
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, marshalerMock *MockMarshaler) {
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
}}

func TestRenewAccount(t *testing.T) {
	for _, tc := range renewAccountTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			req := tc.getReq()

			accountMgr := account.NewMockManager(mockCtrl)
			orderMgr := order.NewMockManager(mockCtrl)
			marshaler := NewMockMarshaler(mockCtrl)
			lndServices := lndclient.LndServices{
				Version: tc.lndVer,
			}
			tc.mockSetter(req, accountMgr, marshaler)

			srv := rpcServer{
				accountManager: accountMgr,
				orderManager:   orderMgr,
				marshaler:      marshaler,
				server: &Server{
					lndServices: &lndclient.GrpcLndServices{
						LndServices: lndServices,
					},
				},
			}
			srv.bestHeight = 100

			ctx, cancel := context.WithTimeout(
				context.Background(), ctxTimeout,
			)
			defer cancel()

			resp, err := srv.RenewAccount(ctx, req)
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)

			err = tc.checkResponse(resp)
			require.NoError(t, err)
		})
	}
}
