package order

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/llm/account"
	"github.com/lightninglabs/llm/clmscript"
	"github.com/lightninglabs/lndclient"
)

// batchSigner is a type that implements the BatchSigner interface and can sign
// for a trader's account inputs in a batch.
type batchSigner struct {
	getAccount func(*btcec.PublicKey) (*account.Account, error)
	signer     lndclient.SignerClient
}

// Sign returns the witness stack of all account inputs in a batch that
// belong to the trader.
//
// NOTE: This method is part of the BatchSigner interface.
func (s *batchSigner) Sign(batch *Batch) (BatchSignature, error) {
	ourSigs := make(BatchSignature)

	// At this point we know that the accounts charged are correct. So we
	// can just go through them, find the corresponding input in the batch
	// TX and sign it.
	for _, acctDiff := range batch.AccountDiffs {
		// Get account from DB and make sure we can create the output.
		acct, err := s.getAccount(acctDiff.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("account not found: %v", err)
		}
		acctOut, err := acct.Output()
		if err != nil {
			return nil, fmt.Errorf("could not get account output: "+
				"%v", err)
		}
		var acctKey [33]byte
		copy(acctKey[:], acct.TraderKey.PubKey.SerializeCompressed())

		// Find the input index we are going to sign.
		inputIndex := -1
		for idx, in := range batch.BatchTX.TxIn {
			if in.PreviousOutPoint == acct.OutPoint {
				inputIndex = idx
			}
		}
		if inputIndex == -1 {
			return nil, fmt.Errorf("account input not found")
		}

		// Gather the remaining components required to sign the
		// transaction and sign it.
		traderKeyTweak := clmscript.TraderKeyTweak(
			acct.BatchKey, acct.Secret, acct.TraderKey.PubKey,
		)
		witnessScript, err := clmscript.AccountWitnessScript(
			acct.Expiry, acct.TraderKey.PubKey, acct.AuctioneerKey,
			acct.BatchKey, acct.Secret,
		)
		if err != nil {
			return nil, err
		}
		signDesc := &lndclient.SignDescriptor{
			KeyDesc:       *acct.TraderKey,
			SingleTweak:   traderKeyTweak,
			WitnessScript: witnessScript,
			Output:        acctOut,
			HashType:      txscript.SigHashAll,
			InputIndex:    inputIndex,
		}
		sigs, err := s.signer.SignOutputRaw(
			context.Background(), batch.BatchTX,
			[]*lndclient.SignDescriptor{signDesc},
		)
		if err != nil {
			return nil, err
		}
		ourSigs[acctKey], err = btcec.ParseDERSignature(sigs[0], btcec.S256())
		if err != nil {
			return nil, err
		}
	}

	return ourSigs, nil
}

// A compile-time constraint to ensure batchSigner implements BatchSigner.
var _ BatchSigner = (*batchSigner)(nil)
