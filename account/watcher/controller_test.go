package watcher

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	gomock "github.com/golang/mock/gomock"
	"github.com/lightninglabs/pool/internal/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errCtrlExpected = errors.New("random error")
)

var controllerLifeCycleTestCases = []struct {
	name          string
	mockSetter    func(mockChainNotifier *test.MockChainNotifierClient)
	expectedError string
}{{
	name: "we are able to start and stop the watcher " +
		"successfully",
	mockSetter: func(mockChainNotifier *test.MockChainNotifierClient) {
		blockChan := make(chan int32)
		errChan := make(chan error)
		mockChainNotifier.EXPECT().
			RegisterBlockEpochNtfn(gomock.Any()).
			Return(blockChan, errChan, nil)
	},
	expectedError: "",
}, {
	name: "unable to start watcher because of " +
		"RegisterBlockEpochNtfn register error",
	mockSetter: func(mockChainNotifier *test.MockChainNotifierClient) {
		blockChan := make(chan int32)
		errChan := make(chan error)
		mockChainNotifier.EXPECT().
			RegisterBlockEpochNtfn(gomock.Any()).
			Return(
				blockChan,
				errChan,
				errCtrlExpected,
			)
	},
	expectedError: errCtrlExpected.Error(),
}}

func TestWatcherControllerLifeCycle(t *testing.T) {
	for _, tc := range controllerLifeCycleTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			chainNotifier := test.NewMockChainNotifierClient(
				mockCtrl,
			)

			tc.mockSetter(chainNotifier)

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
			}

			watcherController := NewController(cfg)

			err := watcherController.Start()
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)

			watcherController.Stop()

			select {
			case <-watcherController.quit:
				return
			case <-time.After(2 * time.Second):
				t.Error("watcher controller not closed on time")
			}
		})
	}
}

var controllerNewBlocksTestCases = []struct {
	name   string
	blocks []int32
}{{
	name: "every time that we receive a new block we update" +
		"our bestHeight and look for overdue expirations",
	blocks: []int32{1, 2, 3},
}}

func TestWatcherControllerNewBlocks(t *testing.T) {
	for _, tc := range controllerNewBlocksTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			blockChan := make(chan int32)
			errChan := make(chan error)
			chainNotifier := test.NewMockChainNotifierClient(
				mockCtrl,
			)

			chainNotifier.EXPECT().
				RegisterBlockEpochNtfn(gomock.Any()).
				Return(blockChan, errChan, nil)

			watcher := NewMockExpiryWatcher(mockCtrl)

			for _, block := range tc.blocks {
				watcher.EXPECT().
					NewBlock(uint32(block))
			}

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
			}

			watcherController := NewController(cfg)
			watcherController.watcher = watcher

			err := watcherController.Start()
			require.NoError(t, err)

			for _, block := range tc.blocks {
				blockChan <- block
			}

			watcherController.Stop()

			select {
			case <-watcherController.quit:
				return
			case <-time.After(2 * time.Second):
				t.Error("new blocks not processed on time")
			}
		})
	}
}

var controllerWatchAccountTestCases = []struct {
	name        string
	expectedErr string
}{{
	name: "Watch account happy path",
	// TODO (positiveblue): add tests for `cancel` logic
}}

func TestWatcherControllerWatchAccount(t *testing.T) {
	traderKeyStr := "036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09"
	traderKeyRaw, _ := hex.DecodeString(traderKeyStr)
	traderKey, _ := btcec.ParsePubKey(traderKeyRaw)

	var txHash chainhash.Hash
	if _, err := rand.Read(txHash[:]); err != nil { // nolint:gosec
		t.Error("unable to create random hash")
	}

	script := make([]byte, 64)
	if _, err := rand.Read(script); err != nil { // nolint:gosec
		t.Error("unable to create random hash")
	}

	numConfs := uint32(6)
	heightHint := uint32(8)

	for _, tc := range controllerWatchAccountTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			confChan := make(chan *chainntnfs.TxConfirmation)
			errChan := make(chan error)

			doneChan := make(chan struct{})

			chainNotifier := test.NewMockChainNotifierClient(
				mockCtrl,
			)

			chainNotifier.EXPECT().
				RegisterBlockEpochNtfn(gomock.Any())

			chainNotifier.EXPECT().
				RegisterConfirmationsNtfn(
					gomock.Any(), &txHash, script,
					int32(numConfs), int32(heightHint),
				).
				Return(confChan, errChan, nil)

			confirmation := &chainntnfs.TxConfirmation{}

			eventHanlers := NewMockEventHandler(mockCtrl)

			eventHanlers.EXPECT().
				HandleAccountConf(traderKey, confirmation).
				Return(nil).
				Do(func(_ *btcec.PublicKey,
					_ *chainntnfs.TxConfirmation) {

					// Close the channel so we signal the
					// test that this function was executed
					close(doneChan)
				})

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
				Handlers:      eventHanlers,
			}

			watcherController := NewController(cfg)

			err := watcherController.Start()
			require.NoError(t, err)

			err = watcherController.WatchAccountConf(
				traderKey, txHash, script, numConfs, heightHint,
			)
			require.NoError(t, err)

			confChan <- confirmation

			select {
			case <-doneChan:
				return
			case <-time.After(2 * time.Second):
				t.Error("confirmation not processed on time")
			}
		})
	}
}

var controllerWatchAccountSpendTestCases = []struct {
	name        string
	expectedErr string
}{{
	name: "Watch account spend happy path",
	// TODO (positiveblue): add tests for `cancel` logic
}}

func TestWatcherControllerWatchAccountSpend(t *testing.T) {
	traderKeyStr := "036b51e0cc2d9e5988ee4967e0ba67ef3727bb633fea21a0af58e0c9395446ba09"
	traderKeyRaw, _ := hex.DecodeString(traderKeyStr)
	traderKey, _ := btcec.ParsePubKey(traderKeyRaw)

	outpoint := wire.OutPoint{}

	script := make([]byte, 64)
	if _, err := rand.Read(script); err != nil { // nolint:gosec
		t.Error("unable to create random hash")
	}

	heightHint := uint32(8)

	for _, tc := range controllerWatchAccountSpendTestCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			spendChan := make(chan *chainntnfs.SpendDetail)
			errChan := make(chan error)

			doneChan := make(chan struct{})

			chainNotifier := test.NewMockChainNotifierClient(
				mockCtrl,
			)

			chainNotifier.EXPECT().
				RegisterBlockEpochNtfn(gomock.Any())

			chainNotifier.EXPECT().
				RegisterSpendNtfn(
					gomock.Any(), &outpoint, script,
					int32(heightHint),
				).
				Return(spendChan, errChan, nil)

			handlers := NewMockEventHandler(mockCtrl)

			spendDetails := &chainntnfs.SpendDetail{}

			handlers.EXPECT().
				HandleAccountSpend(traderKey, spendDetails).
				Return(nil).
				Do(func(_ *btcec.PublicKey,
					_ *chainntnfs.SpendDetail) {

					// Close the channel so we signal the
					// test that this function was executed
					close(doneChan)
				})

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
				Handlers:      handlers,
			}

			watcherController := NewController(cfg)

			err := watcherController.Start()
			require.NoError(t, err)

			err = watcherController.WatchAccountSpend(
				traderKey, outpoint, script, heightHint,
			)
			require.NoError(t, err)

			spendChan <- spendDetails

			select {
			case <-doneChan:
				return
			case <-time.After(2 * time.Second):
				t.Error("spend not processed on time")
			}
		})
	}
}
