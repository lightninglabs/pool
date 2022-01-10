package poolscript

import (
	"bytes"
	"crypto/sha256"
	"math/big"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

const (
	// AccountKeyFamily is the key family used to derive keys which will be
	// used in the 2 of 2 multi-sig construction of a CLM account.
	//
	// TODO(wilmer): decide on actual value.
	AccountKeyFamily keychain.KeyFamily = 220

	// AccountWitnessScriptSize: 79 bytes
	//	- OP_DATA: 1 byte (trader_key length)
	//	- <trader_key>: 33 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- OP_DATA: 1 byte (auctioneer_key length)
	//	- <auctioneer_key>: 33 bytes
	//	- OP_CHECKSIG: 1 byte
	//	- OP_IFDUP: 1 byte
	//	- OP_NOTIF: 1 byte
	//	- OP_DATA: 1 byte (account_expiry length)
	//	- <account_expiry>: 4 bytes
	//	- OP_CHECKLOCKTIMEVERIFY: 1 byte
	//	- OP_ENDIF: 1 byte
	AccountWitnessScriptSize = 1 + 33 + 1 + 1 + 33 + 1 + 1 + 1 + 1 + 4 + 1 + 1

	// MultiSigWitnessSize: 227 bytes
	//      - num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte
	//	- <trader_sig>: 73 bytes
	//	- auctioneer_sig_varint_len: 1 byte
	//	- <auctioneer_sig>: 73 bytes
	//	- witness_script_varint_len: 1 byte
	//	- <witness_script>: 79 bytes
	MultiSigWitnessSize = 1 + 1 + 73 + 1 + 73 + 1 + AccountWitnessScriptSize

	// ExpiryWitnessSize: 154 bytes
	//      - num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte (trader_sig length)
	//	- <trader_sig>: 73 bytes
	//	- witness_script_varint_len: 1 byte (nil length)
	//	- <witness_script>: 79 bytes
	ExpiryWitnessSize = 1 + 1 + 73 + 1 + AccountWitnessScriptSize
)

// TraderKeyTweak computes the tweak based on the current per-batch key and
// shared secret that should be applied to an account's base trader key. The
// tweak is computed as the following:
//
//	tweak = sha256(batchKey || secret || traderKey)
func TraderKeyTweak(batchKey *btcec.PublicKey, secret [32]byte,
	traderKey *btcec.PublicKey) []byte {

	h := sha256.New()
	_, _ = h.Write(batchKey.SerializeCompressed())
	_, _ = h.Write(secret[:])
	_, _ = h.Write(traderKey.SerializeCompressed())
	return h.Sum(nil)
}

// AuctioneerKeyTweak computes the tweak based on the tweaked trader's key that
// should be applied to an account's auctioneer base key. The tweak is computed
// as the following:
//
//	traderKeyTweak = sha256(batchKey || secret || traderKey)
//	tweakedTraderKey = (traderKey + traderKeyTweak) * G
//	auctioneerKeyTweak = sha256(tweakedTraderKey || auctioneerKey)
func AuctioneerKeyTweak(traderKey, auctioneerKey, batchKey *btcec.PublicKey,
	secret [32]byte) []byte {

	traderKeyTweak := TraderKeyTweak(batchKey, secret, traderKey)
	tweakedTraderKey := input.TweakPubKeyWithTweak(traderKey, traderKeyTweak)
	return input.SingleTweakBytes(tweakedTraderKey, auctioneerKey)
}

// AccountWitnessScript returns the witness script of an account.
func AccountWitnessScript(expiry uint32, traderKey, auctioneerKey,
	batchKey *btcec.PublicKey, secret [32]byte) ([]byte, error) {

	traderKeyTweak := TraderKeyTweak(batchKey, secret, traderKey)
	tweakedTraderKey := input.TweakPubKeyWithTweak(traderKey, traderKeyTweak)
	tweakedAuctioneerKey := input.TweakPubKey(auctioneerKey, tweakedTraderKey)

	return accountWitnessScript(
		expiry, tweakedTraderKey.SerializeCompressed(),
		tweakedAuctioneerKey.SerializeCompressed(),
	)
}

// accountWitnessScript returns the witness script of an account from the
// tweaked and serialized trader and auctioneer key.
func accountWitnessScript(expiry uint32, tweakedTraderKey,
	tweakedAuctioneerKey []byte) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddData(tweakedTraderKey)
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddData(tweakedAuctioneerKey)
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_IFDUP)
	builder.AddOp(txscript.OP_NOTIF)
	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
}

// RecoveryHelper is a type that helps speed up account recovery by caching the
// tweaked trader and auctioneer keys for faster script lookups.
type RecoveryHelper struct {
	// TraderKey is the trader's public key.
	TraderKey *btcec.PublicKey

	// AuctioneerKey is the auctioneer's public key.
	AuctioneerKey *btcec.PublicKey

	// BatchKey is the current batch key.
	BatchKey *btcec.PublicKey

	// Secret is the shared secret between trader and auctioneer.
	Secret [32]byte

	tweakedTraderKey     []byte
	tweakedAuctioneerKey []byte
}

// NextBatchKey increments the currently used batch key and re-calculates the
// tweaked keys.
func (r *RecoveryHelper) NextBatchKey() {
	r.BatchKey = IncrementKey(r.BatchKey)

	r.tweakKeys()
}

// NextAccount sets a fresh trader key and secret, then re-calculates the
// tweaked keys.
func (r *RecoveryHelper) NextAccount(traderKey *btcec.PublicKey,
	secret [32]byte) {

	r.TraderKey = traderKey
	r.Secret = secret

	r.tweakKeys()
}

// tweakKeys caches the tweaked keys from the current values.
func (r *RecoveryHelper) tweakKeys() {
	traderKeyTweak := TraderKeyTweak(r.BatchKey, r.Secret, r.TraderKey)
	tweakedTraderKey := input.TweakPubKeyWithTweak(
		r.TraderKey, traderKeyTweak,
	)
	tweakedAuctioneerKey := input.TweakPubKey(
		r.AuctioneerKey, tweakedTraderKey,
	)

	r.tweakedTraderKey = tweakedTraderKey.SerializeCompressed()
	r.tweakedAuctioneerKey = tweakedAuctioneerKey.SerializeCompressed()
}

// LocateOutput looks for an account output in the given transaction that
// corresponds to a script derived with the current settings of the helper and
// the given account expiry.
func (r *RecoveryHelper) LocateOutput(expiry uint32, tx *wire.MsgTx) (uint32,
	bool, error) {

	witnessScript, err := accountWitnessScript(
		expiry, r.tweakedTraderKey, r.tweakedAuctioneerKey,
	)
	if err != nil {
		return 0, false, err
	}

	scriptHash, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return 0, false, err
	}

	idx, ok := LocateOutputScript(tx, scriptHash)
	return idx, ok, nil
}

// LocateAnyOutput looks for an account output in and of the given transactions
// that corresponds to a script derived with the current settings of the helper
// and the given account expiry.
func (r *RecoveryHelper) LocateAnyOutput(expiry uint32,
	txns []*wire.MsgTx) (*wire.MsgTx, uint32, bool, error) {

	for _, tx := range txns {
		// We shouldn't return a pointer to a loop iterator value, so
		// make a copy first.
		tx := tx

		idx, ok, err := r.LocateOutput(expiry, tx)
		if err != nil {
			return nil, 0, false, err
		}

		if ok {
			return tx, idx, true, nil
		}
	}

	return nil, 0, false, nil
}

// AccountScript returns the output script of an account on-chain.
//
// <trader_key> OP_CHECKSIGVERIFY
// <auctioneer_key> OP_CHECKSIG OP_IFDUP OP_NOTIF
//	<account_expiry> OP_CHECKLOCKTIMEVERIFY
// OP_ENDIF
func AccountScript(expiry uint32, traderKey, auctioneerKey,
	batchKey *btcec.PublicKey, secret [32]byte) ([]byte, error) {

	witnessScript, err := AccountWitnessScript(
		expiry, traderKey, auctioneerKey, batchKey, secret,
	)
	if err != nil {
		return nil, err
	}
	return input.WitnessScriptHash(witnessScript)
}

// SpendExpiry returns the witness required to spend an account through the
// expiration script path.
func SpendExpiry(witnessScript, traderSig []byte) wire.TxWitness {
	witness := make(wire.TxWitness, 3)
	witness[0] = nil // Invalid auctioneer sig to force expiry path.
	witness[1] = traderSig
	witness[2] = witnessScript
	return witness
}

// SpendMultiSig returns the witness required to spend an account through the
// multi-sig script path.
func SpendMultiSig(witnessScript, traderSig, auctioneerSig []byte) wire.TxWitness {
	witness := make(wire.TxWitness, 3)
	witness[0] = auctioneerSig
	witness[1] = traderSig
	witness[2] = witnessScript
	return witness
}

// IsExpirySpend determines whether the provided witness corresponds to the
// expiration script path of an account.
func IsExpirySpend(witness wire.TxWitness) bool {
	if len(witness) != 3 {
		return false
	}
	if len(witness[0]) != 0 {
		return false
	}
	return true
}

// IsMultiSigSpend determines whether the provided witness corresponds to the
// multi-sig script path of an account.
func IsMultiSigSpend(witness wire.TxWitness) bool {
	if len(witness) != 3 {
		return false
	}
	if len(witness[0]) == 0 {
		return false
	}
	return true
}

// IncrementKey increments the given key by the backing curve's base point.
func IncrementKey(key *btcec.PublicKey) *btcec.PublicKey {
	curveParams := key.Curve.Params()
	newX, newY := key.Curve.Add(key.X, key.Y, curveParams.Gx, curveParams.Gy)
	return &btcec.PublicKey{
		X:     newX,
		Y:     newY,
		Curve: btcec.S256(),
	}
}

// DecrementKey is the opposite of IncrementKey, it "subtracts one" from the
// current key to arrive at the key used before the IncrementKey operation.
func DecrementKey(key *btcec.PublicKey) *btcec.PublicKey {
	//  priorKey = key - G
	//  priorKey = (key.x, key.y) + (G.x, -G.y)
	curveParams := btcec.S256().Params()
	negY := new(big.Int).Neg(curveParams.Gy)
	negY = negY.Mod(negY, curveParams.P)
	x, y := key.Curve.Add(
		key.X, key.Y, curveParams.Gx, negY,
	)

	return &btcec.PublicKey{
		X:     x,
		Y:     y,
		Curve: btcec.S256(),
	}
}

// LocateOutputScript determines whether a transaction includes an output with a
// specific script. If it does, the output index is returned.
func LocateOutputScript(tx *wire.MsgTx, script []byte) (uint32, bool) {
	for i, txOut := range tx.TxOut {
		if !bytes.Equal(txOut.PkScript, script) {
			continue
		}
		return uint32(i), true
	}
	return 0, false
}

// MatchPreviousOutPoint determines whether or not a PreviousOutPoint appears
// in anye of the provided transactions.
func MatchPreviousOutPoint(op wire.OutPoint, txs []*wire.MsgTx) (*wire.MsgTx,
	bool) {

	for _, tx := range txs {
		tx := tx
		if IncludesPreviousOutPoint(tx, op) {
			return tx, true
		}
	}

	return nil, false
}

// IncludesPreviousOutPoint determines whether a transaction includes a
// given OutPoint as a txIn PreviousOutpoint.
func IncludesPreviousOutPoint(tx *wire.MsgTx, output wire.OutPoint) bool {
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint.Index != output.Index {
			continue
		}

		if !bytes.Equal(txIn.PreviousOutPoint.Hash[:], output.Hash[:]) {
			continue
		}

		return true
	}
	return false
}
