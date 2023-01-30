package order

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/poolscript"
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
func (s *batchSigner) Sign(batch *Batch) (BatchSignature, AccountNonces, error) {
	ourSigs := make(BatchSignature, len(batch.AccountDiffs))
	ourNonces := make(AccountNonces, len(batch.AccountDiffs))

	// At this point we know that the accounts charged are correct. So we
	// can just go through them, find the corresponding input in the batch
	// TX and sign it.
	for _, acctDiff := range batch.AccountDiffs {
		// Get account from DB and make sure we can create the output.
		acct, err := s.getAccount(acctDiff.AccountKey)
		if err != nil {
			return nil, nil, fmt.Errorf("account not found: %v",
				err)
		}
		acctOut, err := acct.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("could not get account "+
				"output: %v", err)
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
			return nil, nil, fmt.Errorf("account input not found")
		}

		// MuSig2 signing works differently!
		if acct.Version >= account.VersionTaprootEnabled {
			partialSig, nonces, err := s.signInputMuSig2(
				acctKey, acct, inputIndex, batch,
			)
			if err != nil {
				return nil, nil, fmt.Errorf("error MuSig2 "+
					"signing input %d: %v", inputIndex, err)
			}

			ourSigs[acctKey] = partialSig
			ourNonces[acctKey] = nonces

			// We're done signing for this account input.
			continue
		}

		// Gather the remaining components required to sign the
		// transaction and sign it.
		traderKeyTweak := poolscript.TraderKeyTweak(
			acct.BatchKey, acct.Secret, acct.TraderKey.PubKey,
		)
		witnessScript, err := poolscript.AccountWitnessScript(
			acct.Expiry, acct.TraderKey.PubKey, acct.AuctioneerKey,
			acct.BatchKey, acct.Secret,
		)
		if err != nil {
			return nil, nil, err
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
			[]*lndclient.SignDescriptor{signDesc}, nil,
		)
		if err != nil {
			return nil, nil, err
		}

		// Make sure the signature is in the expected format (mostly a
		// precaution).
		_, err = ecdsa.ParseDERSignature(sigs[0])
		if err != nil {
			return nil, nil, err
		}

		ourSigs[acctKey] = sigs[0]
	}

	return ourSigs, ourNonces, nil
}

// signInputMuSig2 creates a MuSig2 partial signature for the given account's
// input.
func (s *batchSigner) signInputMuSig2(acctKey [33]byte,
	account *account.Account, accountInputIdx int, batch *Batch) ([]byte,
	poolscript.MuSig2Nonces, error) {

	ctx := context.Background()
	var emptyNonces poolscript.MuSig2Nonces

	serverNonces, ok := batch.ServerNonces[acctKey]
	if !ok {
		return nil, emptyNonces, fmt.Errorf("server didn't include "+
			"nonces for account %x", acctKey[:])
	}

	sessionInfo, cleanup, err := poolscript.TaprootMuSig2SigningSession(
		ctx, account.Version.ScriptVersion(), account.Expiry,
		account.TraderKey.PubKey, account.BatchKey, account.Secret,
		account.AuctioneerKey, s.signer, &account.TraderKey.KeyLocator,
		&serverNonces,
	)
	if err != nil {
		return nil, emptyNonces, fmt.Errorf("error creating MuSig2 "+
			"session: %v", err)
	}

	partialSig, err := poolscript.TaprootMuSig2Sign(
		ctx, accountInputIdx, sessionInfo, s.signer, batch.BatchTX,
		batch.PreviousOutputs, nil, nil,
	)
	if err != nil {
		cleanup()
		return nil, emptyNonces, fmt.Errorf("error signing batch TX: "+
			"%v", err)
	}

	return partialSig, sessionInfo.PublicNonce, nil
}

// A compile-time constraint to ensure batchSigner implements BatchSigner.
var _ BatchSigner = (*batchSigner)(nil)
