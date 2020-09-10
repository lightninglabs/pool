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

	builder := txscript.NewScriptBuilder()

	builder.AddData(tweakedTraderKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddData(tweakedAuctioneerKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_IFDUP)
	builder.AddOp(txscript.OP_NOTIF)
	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_ENDIF)

	return builder.Script()
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
