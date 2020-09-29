package order

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
)

// TestValidateOrderAccountIsolation tests that when we go to validate that an
// account has sufficient order for a balance, we only factor in the oust
// sanding orders for that account, and not all total orders that are open.
func TestValidateOrderAccountIsolation(t *testing.T) {
	t.Parallel()

	orderStore := newMockStore()
	orderManager := NewManager(&ManagerConfig{
		Store: orderStore,
	})

	// We'll now create two accounts, one that's 1 BTC in size, while the
	// other is 2 BTC.
	accountA := account.Account{
		Value: btcutil.SatoshiPerBitcoin,
		TraderKey: &keychain.KeyDescriptor{
			PubKey: acctKeySmall,
		},
	}
	accountB := account.Account{
		Value: btcutil.SatoshiPerBitcoin * 2,
		TraderKey: &keychain.KeyDescriptor{
			PubKey: acctKeyBig,
		},
	}

	// We'll now create an order that will consume at least half (1100
	// units is 1.1 BTC) of account B (it's an ask order).
	var acctKeyB [33]byte
	copy(acctKeyB[:], acctKeyBig.SerializeCompressed())
	orderB := &Ask{
		Kit: newKitFromTemplate(Nonce{0x01}, &Kit{
			State:            StateSubmitted,
			UnitsUnfulfilled: 1100,
			FixedRate:        2_000,
			MaxBatchFeeRate:  1000,
			AcctKey:          acctKeyB,
			LeaseDuration:    144,
		}),
	}

	testTerms := &terms.AuctioneerTerms{
		MaxOrderDuration: 100 * MinimumOrderDurationBlocks,
		OrderExecBaseFee: 1,
		OrderExecFeeRate: 100,
	}

	// Submitting this order for account B should pass validation.
	err := orderManager.validateOrder(orderB, &accountB, testTerms)
	if err != nil {
		t.Fatalf("order B validation failed: %v", err)
	}

	// Simulating order acceptance, we'll now add this order to the order
	// store.
	if err := orderStore.SubmitOrder(orderB); err != nil {
		t.Fatalf("unable to submit order: %v", err)
	}

	// Next, we'll create a ask that'll consume _most_ (but not all) of account
	// A's balance. This should pass validation as although account B can't
	// handle the order account A can.
	var acctKeyA [33]byte
	copy(acctKeyA[:], acctKeySmall.SerializeCompressed())
	orderA := &Ask{
		Kit: newKitFromTemplate(Nonce{0x01}, &Kit{
			State:            StateSubmitted,
			UnitsUnfulfilled: 800,
			FixedRate:        2_000,
			MaxBatchFeeRate:  1000,
			AcctKey:          acctKeyA,
			LeaseDuration:    144,
		}),
	}

	err = orderManager.validateOrder(orderA, &accountA, testTerms)
	if err != nil {
		t.Fatalf("order A failed validation: %v", err)
	}
}
