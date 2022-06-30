package poolscript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
)

const (
	numOperations          = 10000
	numOperationsQuickTest = 1000
	oddByte                = input.PubKeyFormatCompressedOdd
)

var (
	// initialBatchKey is the hard coded starting point for the auctioneer's
	// batch key in every environment. Copied here to avoid circular
	// dependency with the account package.
	initialBatchKeyBytes, _ = hex.DecodeString(
		"02824d0cbac65e01712124c50ff2cc74ce22851d7b444c1bf2ae66afefb8" +
			"eaf27f",
	)

	// batchKeyIncremented1kTimesBytes is the initial batch keys incremented
	// by G 10000 times.
	batchKeyIncremented10kTimesBytes, _ = hex.DecodeString(
		"03d9dfc4971c9cbabb1b9a4c991914211aa21286e007c15d7e9d828da0b8" +
			"f07763",
	)

	sharedSecret   = [32]byte{11, 22, 33, 44, 55}
	expiry         = uint32(144 * 365)
	batchPubKey, _ = btcec.ParsePubKey(
		initialBatchKeyBytes,
	)
)

// TestIncrementDecrementKey makes sure that incrementing and decrementing an EC
// public key are inverse operations to each other.
func TestIncrementDecrementKey(t *testing.T) {
	t.Parallel()

	rand.Seed(time.Now().Unix())

	type byteInput [32]byte
	mainScenario := func(b byteInput) bool {
		_, randomStartBatchKey := btcec.PrivKeyFromBytes(b[:])

		// Increment the key numOperations times.
		currentKey := randomStartBatchKey
		for i := 0; i < numOperationsQuickTest; i++ {
			currentKey = IncrementKey(currentKey)
		}

		// Decrement the key again.
		for i := 0; i < numOperationsQuickTest; i++ {
			currentKey = DecrementKey(currentKey)
		}

		// We should arrive at the same start key again.
		return randomStartBatchKey.IsEqual(currentKey)
	}

	require.NoError(t, quick.Check(mainScenario, nil))
}

// TestIncrementBatchKey tests that incrementing the static, hard-coded batch
// key 1000 times gives a specific key and decrementing the same number of times
// gives the batch key again.
func TestIncrementBatchKey(t *testing.T) {
	t.Parallel()

	startBatchKey, err := btcec.ParsePubKey(initialBatchKeyBytes)
	require.NoError(t, err)

	batchKeyIncremented10kTimes, err := btcec.ParsePubKey(
		batchKeyIncremented10kTimesBytes,
	)
	require.NoError(t, err)

	currentKey := startBatchKey
	for i := 0; i < numOperations; i++ {
		currentKey = IncrementKey(currentKey)
	}

	require.Equal(t, batchKeyIncremented10kTimes, currentKey)

	for i := 0; i < numOperations; i++ {
		currentKey = DecrementKey(currentKey)
	}

	require.Equal(t, startBatchKey, currentKey)
}

// FuzzWitnessSpendDetection fuzz tests the witness spend detection functions.
func FuzzWitnessSpendDetection(f *testing.F) {
	f.Fuzz(func(t *testing.T, a, b, c, d []byte, num uint8) {
		witness := make([][]byte, num)

		if num > 0 {
			witness[0] = a
		}
		if num > 1 {
			witness[1] = b
		}
		if num > 2 {
			witness[2] = c
		}
		if num > 3 {
			witness[3] = d
		}
		_ = IsMultiSigSpend(witness)
		_ = IsExpirySpend(witness)
		_ = IsTaprootMultiSigSpend(witness)
		_ = IsTaprootExpirySpend(witness)
	})
}

// TestWitnessCorrectness tests that our witness sizes are correct and that they
// can actually spend an output of the given type.
func TestWitnessCorrectness(t *testing.T) {
	t.Parallel()

	dummyHash := sha256.Sum256([]byte("imagine this was a transaction"))
	testCases := []struct {
		name         string
		version      Version
		timeout      bool
		expectedSize int
		witness      func(t *testing.T, trader,
			auctioneer *btcec.PrivateKey) wire.TxWitness
		check func(witness wire.TxWitness) bool
	}{{
		name:         "v0 multisig",
		version:      VersionWitnessScript,
		expectedSize: MultiSigWitnessSize,
		witness: func(t *testing.T, trader,
			auctioneer *btcec.PrivateKey) wire.TxWitness {

			script, err := AccountWitnessScript(
				expiry, trader.PubKey(), auctioneer.PubKey(),
				batchPubKey, sharedSecret,
			)
			require.NoError(t, err)

			traderSig := ecdsa.Sign(trader, dummyHash[:])
			auctioneerSig := ecdsa.Sign(auctioneer, dummyHash[:])
			return SpendMultiSig(
				script, serializeSigHashAll(traderSig),
				serializeSigHashAll(auctioneerSig),
			)
		},
		check: IsMultiSigSpend,
	}, {
		name:         "v0 timeout",
		version:      VersionWitnessScript,
		timeout:      true,
		expectedSize: ExpiryWitnessSize,
		witness: func(t *testing.T, trader,
			auctioneer *btcec.PrivateKey) wire.TxWitness {

			script, err := AccountWitnessScript(
				expiry, trader.PubKey(), auctioneer.PubKey(),
				batchPubKey, sharedSecret,
			)
			require.NoError(t, err)

			traderSig := ecdsa.Sign(trader, dummyHash[:])
			return SpendExpiry(
				script, serializeSigHashAll(traderSig),
			)
		},
		check: IsExpirySpend,
	}, {
		name:         "v1 multisig",
		version:      VersionTaprootMuSig2,
		expectedSize: TaprootMultiSigWitnessSize,
		witness: func(t *testing.T, trader,
			auctioneer *btcec.PrivateKey) wire.TxWitness {

			traderSig, err := schnorr.Sign(trader, dummyHash[:])
			require.NoError(t, err)
			return SpendMuSig2Taproot(traderSig.Serialize())
		},
		check: IsTaprootMultiSigSpend,
	}, {
		name:         "v1 timeout",
		version:      VersionTaprootMuSig2,
		timeout:      true,
		expectedSize: TaprootExpiryWitnessSize,
		witness: func(t *testing.T, trader,
			auctioneer *btcec.PrivateKey) wire.TxWitness {

			auctioneerPub := auctioneer.PubKey()
			_, tapLeaf, err := TaprootKey(
				expiry, trader.PubKey(), auctioneerPub,
				batchPubKey, sharedSecret,
			)
			require.NoError(t, err)

			traderSig, err := schnorr.Sign(trader, dummyHash[:])
			require.NoError(t, err)

			odd := auctioneerPub.SerializeCompressed()[0] == oddByte
			controlBlock := txscript.ControlBlock{
				InternalKey:     auctioneerPub,
				LeafVersion:     txscript.BaseLeafVersion,
				OutputKeyYIsOdd: odd,
			}
			blockBytes, err := controlBlock.ToBytes()
			require.NoError(t, err)

			return SpendExpiryTaproot(
				tapLeaf.Script, traderSig.Serialize(),
				blockBytes,
			)
		},
		check: IsTaprootExpirySpend,
	}}

	scenario := func(trader, auctioneer *btcec.PrivateKey) bool {
		for _, tc := range testCases {
			txWitness := tc.witness(t, trader, auctioneer)
			witness, err := serializeTxWitness(txWitness)
			if err != nil {
				t.Logf("Unexpected error: %v", err)
				return false
			}

			// For Taproot scripts we can actually enforce exact
			// witness size estimations! The only variable size item
			// is the expiry because that's encoded as a VarInt. But
			// we chose an expiry >32k for this test to enforce the
			// 4-byte serialization.
			if tc.version == VersionTaprootMuSig2 {
				if len(witness) != tc.expectedSize {
					t.Logf("Unexpected witness size %d: %x",
						len(witness), witness)
					return false
				}
			} else {
				if len(witness) > tc.expectedSize {
					t.Logf("Unexpected witness size %d: %x",
						len(witness), witness)
					return false
				}
			}

			passesCheck := tc.check(txWitness)
			if !passesCheck {
				t.Logf("Did not pass check, trader key %x, "+
					"auctioneer key %x", trader.Serialize(),
					auctioneer.Serialize())
				return false
			}
		}

		return true
	}
	quickCfg := &quick.Config{
		MaxCount: 1000,
		Values: func(values []reflect.Value, r *rand.Rand) {
			pkBytes := make([]byte, 32)
			_, _ = r.Read(pkBytes)
			_, _ = r.Read(pkBytes)
			_, _ = r.Read(pkBytes)
			trader, _ := btcec.PrivKeyFromBytes(pkBytes)
			_, _ = r.Read(pkBytes)
			auctioneer, _ := btcec.PrivKeyFromBytes(pkBytes)

			values[1] = reflect.ValueOf(trader)
			values[0] = reflect.ValueOf(auctioneer)
		},
	}
	require.NoError(t, quick.Check(scenario, quickCfg))
}

// TestTaprootSpend tests that the taproot key and script spends can be executed
// correctly.
func TestTaprootSpend(t *testing.T) {
	t.Parallel()

	t.Run("MuSig2", func(tt *testing.T) {
		testTaprootSpend(tt, false)
	})
	t.Run("Expiry", func(tt *testing.T) {
		testTaprootSpend(tt, true)
	})
}

// testTaprootSpend executes a Taproot spend, either using the MuSig2 key spend
// path or the expiry script path.
func testTaprootSpend(t *testing.T, expiryPath bool) {
	trader, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	traderPub := trader.PubKey()

	auctioneer, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	auctioneerPub := auctioneer.PubKey()

	const outputSize = 2000000

	tx := wire.NewMsgTx(2)
	tx.LockTime = expiry
	tx.TxIn = []*wire.TxIn{{
		PreviousOutPoint: wire.OutPoint{
			Hash:  [32]byte{1, 2, 3},
			Index: 2,
		},
	}}

	taprootKey, tapLeaf, err := TaprootKey(
		expiry, traderPub, auctioneerPub, batchPubKey, sharedSecret,
	)
	require.NoError(t, err)

	pkScript, err := AccountScript(
		VersionTaprootMuSig2, expiry, traderPub, auctioneerPub,
		batchPubKey, sharedSecret,
	)
	require.NoError(t, err)
	tx.TxOut = []*wire.TxOut{{
		Value:    outputSize - 800,
		PkScript: pkScript,
	}}

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(
		pkScript, outputSize,
	)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutputFetcher)

	if expiryPath {
		// For the expiry path we sign the tap script sighash with the
		// tweaked trader key.
		sigHash, err := txscript.CalcTapscriptSignaturehash(
			sigHashes, txscript.SigHashDefault, tx, 0,
			prevOutputFetcher, *tapLeaf,
		)
		require.NoError(t, err)

		traderKeyTweak := TraderKeyTweak(
			batchPubKey, sharedSecret, traderPub,
		)
		traderTweaked := input.TweakPrivKey(trader, traderKeyTweak)
		traderSig, err := schnorr.Sign(traderTweaked, sigHash)
		require.NoError(t, err)

		odd := taprootKey.FinalKey.SerializeCompressed()[0] == oddByte
		controlBlock := txscript.ControlBlock{
			InternalKey:     taprootKey.PreTweakedKey,
			LeafVersion:     txscript.BaseLeafVersion,
			OutputKeyYIsOdd: odd,
		}
		blockBytes, err := controlBlock.ToBytes()
		require.NoError(t, err)

		tx.TxIn[0].Witness = SpendExpiryTaproot(
			tapLeaf.Script, traderSig.Serialize(), blockBytes,
		)
	} else {
		// For the MuSig2 key spend path we sign the normal Taproot
		// sighash with the combined MuSig2 key.
		sigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault, tx, 0,
			prevOutputFetcher,
		)
		require.NoError(t, err)

		traderPubSchnorr, _ := schnorr.ParsePubKey(
			schnorr.SerializePubKey(traderPub),
		)
		auctioneerPubSchnorr, _ := schnorr.ParsePubKey(
			schnorr.SerializePubKey(auctioneerPub),
		)
		signerKeys := []*btcec.PublicKey{
			traderPubSchnorr, auctioneerPubSchnorr,
		}

		rootHash := tapLeaf.TapHash()
		rootHashOpt := musig2.WithTaprootTweakCtx(rootHash[:])
		signerOpts := musig2.WithKnownSigners(signerKeys)

		traderCtx, err := musig2.NewContext(
			trader, true, rootHashOpt, signerOpts,
		)
		require.NoError(t, err)
		auctioneerCtx, err := musig2.NewContext(
			auctioneer, true, rootHashOpt, signerOpts,
		)
		require.NoError(t, err)

		traderSession, err := traderCtx.NewSession()
		require.NoError(t, err)
		auctioneerSession, err := auctioneerCtx.NewSession()
		require.NoError(t, err)

		allNonces, err := traderSession.RegisterPubNonce(
			auctioneerSession.PublicNonce(),
		)
		require.NoError(t, err)
		require.True(t, allNonces)
		allNonces, err = auctioneerSession.RegisterPubNonce(
			traderSession.PublicNonce(),
		)
		require.NoError(t, err)
		require.True(t, allNonces)

		var msg [32]byte
		copy(msg[:], sigHash)
		traderSig, err := traderSession.Sign(msg)
		require.NoError(t, err)

		_, err = auctioneerSession.Sign(msg)
		require.NoError(t, err)

		fullSigOk, err := auctioneerSession.CombineSig(traderSig)
		require.NoError(t, err)
		require.True(t, fullSigOk)

		fullSig := auctioneerSession.FinalSig()
		tx.TxIn[0].Witness = SpendMuSig2Taproot(fullSig.Serialize())
	}

	vm, err := txscript.NewEngine(
		pkScript, tx, 0, txscript.StandardVerifyFlags, nil, sigHashes,
		outputSize, txscript.NewCannedPrevOutputFetcher(
			pkScript, outputSize,
		),
	)
	require.NoError(t, err)
	err = vm.Execute()
	require.NoError(t, err, "invalid witness")
}

// serializeSigHash serializes the given signature to its raw byte form and also
// appends the txscript.SigHashAll flag.
func serializeSigHashAll(s input.Signature) []byte {
	return append(s.Serialize(), byte(txscript.SigHashAll))
}

// serializeTxWitness return the wire witness stack into raw bytes.
func serializeTxWitness(txWitness wire.TxWitness) ([]byte, error) {
	var witnessBytes bytes.Buffer
	err := psbt.WriteTxWitness(&witnessBytes, txWitness)
	if err != nil {
		return nil, fmt.Errorf("error serializing witness: %v", err)
	}

	return witnessBytes.Bytes(), nil
}
