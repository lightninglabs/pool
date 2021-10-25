package watcher

import (
	"errors"
	"testing"
	"time"

	gomock "github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcherControllerLifeCycle(t *testing.T) {
	var (
		chainNotifierErr = errors.New("unable to RegisterBlockEpochNtfn")
	)

	tt := []struct {
		name       string
		mockSetter func(
			mockChainNotifier *MockChainNotifierClient,
		)
		expectedError string
	}{
		{
			name: "we are able to start and stop the watcher " +
				"successfully",
			mockSetter: func(
				mockChainNotifier *MockChainNotifierClient,
			) {
				blockChan := make(chan int32)
				errChan := make(chan error)
				mockChainNotifier.EXPECT().
					RegisterBlockEpochNtfn(gomock.Any()).
					Return(blockChan, errChan, nil)

			},
			expectedError: "",
		},
		{
			name: "unable to start watcher because of " +
				"RegisterBlockEpochNtfn register error",
			mockSetter: func(
				mockChainNotifier *MockChainNotifierClient,
			) {

				blockChan := make(chan int32)
				errChan := make(chan error)
				mockChainNotifier.EXPECT().
					RegisterBlockEpochNtfn(gomock.Any()).
					Return(blockChan, errChan, chainNotifierErr)
			},
			expectedError: chainNotifierErr.Error(),
		},
	}

	for _, tc := range tt {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			chainNotifier := NewMockChainNotifierClient(mockCtrl)
			executor := NewMockExecutor(mockCtrl)
			tc.mockSetter(chainNotifier)

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
			}

			watcherController := NewWatcherController(
				executor,
				cfg,
			)

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

func TestWatcherControllerNewBlocks(t *testing.T) {
	tt := []struct {
		name          string
		blocks        []int32
		expectedError string
	}{
		{
			name: "every time that we receive a new block we " +
				"update our bestHeight and look for " +
				"overdue expirations",
			blocks: []int32{1, 2, 3},
		},
	}

	for _, tc := range tt {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			mockCtrl := gomock.NewController(t)
			defer mockCtrl.Finish()

			blockChan := make(chan int32)
			errChan := make(chan error)
			chainNotifier := NewMockChainNotifierClient(mockCtrl)

			chainNotifier.EXPECT().
				RegisterBlockEpochNtfn(gomock.Any()).
				Return(blockChan, errChan, nil)

			executor := NewMockExecutor(mockCtrl)

			for _, block := range tc.blocks {
				executor.EXPECT().
					NewBlock(uint32(block))
				executor.EXPECT().
					ExecuteOverdueExpirations(uint32(block))
			}

			cfg := &CtrlConfig{
				ChainNotifier: chainNotifier,
			}

			watcherController := NewWatcherController(
				executor,
				cfg,
			)

			err := watcherController.Start()
			if tc.expectedError != "" {
				assert.EqualError(t, err, tc.expectedError)
				return
			}
			require.NoError(t, err)

			for _, block := range tc.blocks {
				blockChan <- block
			}

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
