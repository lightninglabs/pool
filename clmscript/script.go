package clmscript

import (
	"bytes"

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
)

// AccountScript returns the witness script of an account on-chain.
//
// OP_IF
//	<account expiry>
//	OP_CHECKLOCKTIMEVERIFY
//	OP_DROP
//	<trader key>
//	OP_CHECKSIG
// OP_ELSE
//	OP_2 <trader key> <auctioneer key> OP_2
//	OP_CHECKMULTISIG
// OP_ENDIF
func AccountScript(expiry uint32, traderKey, auctioneerKey *btcec.PublicKey) ([]byte, error) {
	builder := txscript.NewScriptBuilder()

	builder.AddOp(txscript.OP_IF)

	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)
	builder.AddOp(txscript.OP_DROP)
	builder.AddData(traderKey.SerializeCompressed())
	builder.AddOp(txscript.OP_CHECKSIG)

	builder.AddOp(txscript.OP_ELSE)

	builder.AddOp(txscript.OP_2)
	builder.AddData(traderKey.SerializeCompressed())
	builder.AddData(auctioneerKey.SerializeCompressed())
	builder.AddOp(txscript.OP_2)
	builder.AddOp(txscript.OP_CHECKMULTISIG)

	builder.AddOp(txscript.OP_ENDIF)

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}
	return input.WitnessScriptHash(script)
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
