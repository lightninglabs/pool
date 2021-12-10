package pool

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	gomock "github.com/golang/mock/gomock"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errExpected     = errors.New("random error")
	ctxTimeout      = 1 * time.Second
	traderKeyStr    = "036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09"
	traderKeyRaw, _ = hex.DecodeString(traderKeyStr)
)

func getAccountKey(acctKey []byte) *btcec.PublicKey {
	res, _ := btcec.ParsePubKey(acctKey, btcec.S256())
	return res
}

func genRenewAccountReq(accountKey []byte, absolute, relative uint32,
	feerate uint64) *poolrpc.RenewAccountRequest {

	req := &poolrpc.RenewAccountRequest{}

	req.AccountKey = accountKey
	req.FeeRateSatPerKw = feerate
	if absolute != 0 {
		req.AccountExpiry = &poolrpc.RenewAccountRequest_AbsoluteExpiry{
			AbsoluteExpiry: absolute,
		}
	} else {
		req.AccountExpiry = &poolrpc.RenewAccountRequest_RelativeExpiry{
			RelativeExpiry: relative,
		}
	}

	return req
}

var renewAccountTestCases = []struct {
	name          string
	getReq        func() *poolrpc.RenewAccountRequest
	checkResponse func(*poolrpc.RenewAccountResponse) error
	mockSetter    func(*poolrpc.RenewAccountRequest,
		*account.MockManager, *order.MockManager,
		*MockMarshaler,
	)
	expectedError string
}{{
	name: "we are able to successfully renew an account",
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 0, 10, 1000)
	},
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, odrMgr *order.MockManager,
		marshalerMock *MockMarshaler,
	) {

		hasPendingBatch := false
		hasPendingExtension := false

		odrMgr.EXPECT().
			HasPendingBatch().
			Return(hasPendingBatch)

		// Renew account params
		bestHeight := uint32(100)
		feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
		expiryHeight := req.GetAbsoluteExpiry()
		if expiryHeight == 0 {
			expiryHeight = 100 + req.GetRelativeExpiry()
		}
		// RenewAccount returns
		acc := &account.Account{}
		tx := &wire.MsgTx{}
		accMgr.EXPECT().
			RenewAccount(
				gomock.Any(), getAccountKey(req.AccountKey),
				expiryHeight, feeRate, bestHeight,
				hasPendingExtension,
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
	name: "accounts participating in a batch WITHOUT a pending expiry " +
		"extension can renew their account",
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 0, 10, 1000)
	},
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, odrMgr *order.MockManager,
		marshalerMock *MockMarshaler,
	) {
		accountKey := getAccountKey(req.AccountKey)
		hasPendingBatch := true
		hasPendingExtension := false

		newExpiry := uint32(0)

		odrMgr.EXPECT().
			HasPendingBatch().
			Return(hasPendingBatch)

		odrMgr.EXPECT().
			PendingBatch().
			Return(&order.Batch{
				AccountDiffs: []*order.AccountDiff{
					{
						AccountKey: accountKey,
						NewExpiry:  newExpiry,
					},
				},
			})

		// Renew account params
		bestHeight := uint32(100)
		feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
		expiryHeight := req.GetAbsoluteExpiry()
		if expiryHeight == 0 {
			expiryHeight = 100 + req.GetRelativeExpiry()
		}
		// RenewAccount returns
		acc := &account.Account{}
		tx := &wire.MsgTx{}
		accMgr.EXPECT().
			RenewAccount(
				gomock.Any(), accountKey,
				expiryHeight, feeRate, bestHeight,
				hasPendingExtension,
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
	name: "accounts participating in a batch WITH a pending expiry " +
		"extension cannot renew their account",
	getReq: func() *poolrpc.RenewAccountRequest {
		return genRenewAccountReq(traderKeyRaw, 0, 10, 1000)
	},
	mockSetter: func(req *poolrpc.RenewAccountRequest,
		accMgr *account.MockManager, odrMgr *order.MockManager,
		marshalerMock *MockMarshaler,
	) {
		accountKey := getAccountKey(req.AccountKey)
		hasPendingBatch := true
		hasPendingExtension := true
		newExpiry := uint32(120)

		odrMgr.EXPECT().
			HasPendingBatch().
			Return(hasPendingBatch)

		odrMgr.EXPECT().
			PendingBatch().
			Return(&order.Batch{
				AccountDiffs: []*order.AccountDiff{
					{
						AccountKey: accountKey,
						NewExpiry:  newExpiry,
					},
				},
			})

		// Renew account params
		bestHeight := uint32(100)
		feeRate := chainfee.SatPerKWeight(req.FeeRateSatPerKw)
		expiryHeight := req.GetAbsoluteExpiry()
		if expiryHeight == 0 {
			expiryHeight = 100 + req.GetRelativeExpiry()
		}
		// RenewAccount returns
		accMgr.EXPECT().
			RenewAccount(
				gomock.Any(), getAccountKey(req.AccountKey),
				expiryHeight, feeRate, bestHeight,
				hasPendingExtension,
			).
			Return(nil, nil, errExpected)
	},
	checkResponse: func(*poolrpc.RenewAccountResponse) error {
		return nil
	},
	expectedError: errExpected.Error(),
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
			tc.mockSetter(req, accountMgr, orderMgr, marshaler)

			srv := rpcServer{
				accountManager: accountMgr,
				orderManager:   orderMgr,
				marshaler:      marshaler,
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
