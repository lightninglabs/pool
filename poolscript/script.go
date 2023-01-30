package poolscript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
)

// Version represents the type of Pool account script that is used for either
// the trader or auctioneer accounts.
type Version uint8

const (
	// VersionWitnessScript is the legacy script version that used a single
	// p2wsh script for both spend paths.
	VersionWitnessScript Version = 0

	// VersionTaprootMuSig2 is the script version that uses a MuSig2
	// combined key of the auctioneer's and trader's public keys as the
	// internal key and a single script leaf of the expiry path as the
	// taproot script tree merkle root.
	VersionTaprootMuSig2 Version = 1

	// VersionTaprootMuSig2V100RC2 is the script version that uses the
	// MuSig2 protocol v1.0.0-rc2 for creating the MuSig2 combined internal
	// key but is otherwise identical to VersionTaprootMuSig2.
	VersionTaprootMuSig2V100RC2 Version = 2

	// AccountKeyFamily is the key family used to derive keys which will be
	// used in the 2 of 2 multi-sig construction of a CLM account.
	AccountKeyFamily keychain.KeyFamily = 220

	// MaxWitnessSigLen is the maximum length of a DER encoded signature and
	// is when both R and S are 33 bytes each and the sighash flag is
	// appended to it. R and S can be 33 bytes because a 256-bit integer
	// requires 32 bytes and an additional leading null byte might be
	// required if the high bit is set in the value.
	//
	// 0x30 + <1-byte> + 0x02 + 0x21 + <33 bytes> + 0x2 + 0x21 + <33 bytes>.
	MaxWitnessSigLen = 72 + 1

	// AccountWitnessScriptSize evaluates to 79 bytes:
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

	// MultiSigWitnessSize evaluates to 227 bytes:
	//	- num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte
	//	- <trader_sig>: 73 bytes
	//	- auctioneer_sig_varint_len: 1 byte
	//	- <auctioneer_sig>: 73 bytes
	//	- witness_script_varint_len: 1 byte
	//	- <witness_script>: 79 bytes
	MultiSigWitnessSize = 1 + 1 + MaxWitnessSigLen + 1 + MaxWitnessSigLen +
		1 + AccountWitnessScriptSize

	// ExpiryWitnessSize evaluates to 154 bytes:
	//	- num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte (trader_sig length)
	//	- <trader_sig>: 73 bytes
	//	- witness_script_varint_len: 1 byte (nil length)
	//	- <witness_script>: 79 bytes
	ExpiryWitnessSize = 1 + 1 + MaxWitnessSigLen +
		1 + AccountWitnessScriptSize

	// TaprootMultiSigWitnessSize evaluates to 66 bytes:
	//	- num_witness_elements: 1 byte
	//	- sig_varint_len: 1 byte
	//	- <sig>: 64 bytes
	TaprootMultiSigWitnessSize = 1 + 1 + 64

	// TaprootExpiryScriptSize evaluates to 39 bytes:
	//	- OP_DATA: 1 byte (trader_key length)
	//	- <trader_key>: 32 bytes
	//	- OP_CHECKSIGVERIFY: 1 byte
	//	- <account_expiry>: 4 bytes
	//	- OP_CHECKLOCKTIMEVERIFY: 1 byte
	TaprootExpiryScriptSize = 1 + 32 + 1 + 4 + 1

	// TaprootExpiryWitnessSize evaluates to 140 bytes:
	//	- num_witness_elements: 1 byte
	//	- trader_sig_varint_len: 1 byte (trader_sig length)
	//	- <trader_sig>: 64 bytes
	//	- witness_script_varint_len: 1 byte (script length)
	//	- <witness_script>: 39 bytes
	//	- control_block_varint_len: 1 byte (control block length)
	//	- <control_block>: 33 bytes
	TaprootExpiryWitnessSize = 1 + 1 + 64 + 1 + TaprootExpiryScriptSize + 1 + 33
)

// MuSig2Nonces is a type for a MuSig2 nonce pair (2 times 33-byte).
type MuSig2Nonces [musig2.PubNonceSize]byte

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
// For version 0 (p2wsh) this returns the hash of the following script:
//
//	<trader_key> OP_CHECKSIGVERIFY
//	<auctioneer_key> OP_CHECKSIG OP_IFDUP OP_NOTIF
//	        <account_expiry> OP_CHECKLOCKTIMEVERIFY
//	OP_ENDIF
//
// For version 1 (p2tr) this returns the taproot key of a MuSig2 combined key
// of the auctioneer's and trader's public keys as the internal key, tweaked
// with the hash of a single script leaf that has the following script:
// <trader_key> OP_CHECKSIGVERIFY <account_expiry> OP_CHECKLOCKTIMEVERIFY.
func AccountScript(version Version, expiry uint32, traderKey, auctioneerKey,
	batchKey *btcec.PublicKey, secret [32]byte) ([]byte, error) {

	switch version {
	case VersionWitnessScript:
		witnessScript, err := AccountWitnessScript(
			expiry, traderKey, auctioneerKey, batchKey, secret,
		)
		if err != nil {
			return nil, err
		}
		return input.WitnessScriptHash(witnessScript)

	case VersionTaprootMuSig2, VersionTaprootMuSig2V100RC2:
		aggregateKey, _, err := TaprootKey(
			version, expiry, traderKey, auctioneerKey, batchKey,
			secret,
		)
		if err != nil {
			return nil, err
		}

		return txscript.PayToTaprootScript(aggregateKey.FinalKey)

	default:
		return nil, fmt.Errorf("invalid script version <%d>", version)
	}
}

// TaprootKey returns the aggregated MuSig2 combined internal key and the
// tweaked Taproot key of an account output, as well as the expiry script tap
// leaf.
func TaprootKey(scriptVersion Version, expiry uint32, traderKey, auctioneerKey,
	batchKey *btcec.PublicKey, secret [32]byte) (*musig2.AggregateKey,
	*txscript.TapLeaf, error) {

	expiryLeaf, err := TaprootExpiryScript(
		expiry, traderKey, batchKey, secret,
	)
	if err != nil {
		return nil, nil, err
	}

	var muSig2Version input.MuSig2Version
	switch scriptVersion {
	// The v0.4.0 MuSig2 implementation requires the keys to be serialized
	// using the Schnorr (32-byte x-only) serialization format.
	case VersionTaprootMuSig2:
		muSig2Version = input.MuSig2Version040

		auctioneerKey, err = schnorr.ParsePubKey(
			schnorr.SerializePubKey(auctioneerKey),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing auctioneer "+
				"key: %w", err)
		}

		traderKey, err = schnorr.ParsePubKey(
			schnorr.SerializePubKey(traderKey),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("error parsing trader "+
				"key: %w", err)
		}

	// The v1.0.0-rc2 MuSig2 implementation works with the regular, 33-byte
	// compressed keys, so we can just pass them in as they are.
	case VersionTaprootMuSig2V100RC2:
		muSig2Version = input.MuSig2Version100RC2

	default:
		return nil, nil, fmt.Errorf("invalid account version <%d>",
			scriptVersion)
	}

	rootHash := expiryLeaf.TapHash()
	aggregateKey, err := input.MuSig2CombineKeys(
		muSig2Version, []*btcec.PublicKey{auctioneerKey, traderKey},
		true, &input.MuSig2Tweaks{
			TaprootTweak: rootHash[:],
		},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error combining keys: %v", err)
	}

	return aggregateKey, expiryLeaf, nil
}

// TaprootExpiryScript returns the leaf script of the expiry script path.
//
// <trader_key> OP_CHECKSIGVERIFY <account_expiry> OP_CHECKLOCKTIMEVERIFY.
func TaprootExpiryScript(expiry uint32, traderKey, batchKey *btcec.PublicKey,
	secret [32]byte) (*txscript.TapLeaf, error) {

	traderKeyTweak := TraderKeyTweak(batchKey, secret, traderKey)
	tweakedTraderKey := input.TweakPubKeyWithTweak(
		traderKey, traderKeyTweak,
	)

	builder := txscript.NewScriptBuilder()

	builder.AddData(schnorr.SerializePubKey(tweakedTraderKey))
	builder.AddOp(txscript.OP_CHECKSIGVERIFY)

	builder.AddInt64(int64(expiry))
	builder.AddOp(txscript.OP_CHECKLOCKTIMEVERIFY)

	script, err := builder.Script()
	if err != nil {
		return nil, err
	}

	leaf := txscript.NewBaseTapLeaf(script)
	return &leaf, nil
}

// SpendExpiryTaproot returns the witness required to spend an account through
// the expiration script path of a tapscript spend.
func SpendExpiryTaproot(witnessScript, traderSig,
	serializedControlBlock []byte) wire.TxWitness {

	witness := make(wire.TxWitness, 3)
	witness[0] = traderSig
	witness[1] = witnessScript
	witness[2] = serializedControlBlock
	return witness
}

// SpendMuSig2Taproot returns the witness required to spend an account through
// the internal key which is a MuSig2 combined key that requires a single
// Schnorr signature.
func SpendMuSig2Taproot(combinedSig []byte) wire.TxWitness {
	witness := make(wire.TxWitness, 1)
	witness[0] = combinedSig
	return witness
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

// IsTaprootExpirySpend determines whether the provided witness corresponds to
// the expiration script path of a Taproot enabled (version 1) account.
func IsTaprootExpirySpend(witness wire.TxWitness) bool {
	// At a minimum we expect these three elements: the trader's signature,
	// the script, and the control block. We also accept an additional
	// optional fourth element: the annex.
	if hasAnnex(witness) {
		// Remove the annex element. If the annex is present, it is
		// always the last element.
		witness = witness[:len(witness)-1]
	}

	if len(witness) != 3 {
		return false
	}

	// An expiry spend is a script spend and therefore always has to have a
	// control block. And a valid control block is at _least_ 33 bytes long
	// (leaf version of 1 byte and the 32-byte x-only internal key).
	ctrlBlock := witness[2]
	if len(ctrlBlock) < 33 {
		return false
	}

	// The expiry is the only variably-sized part in the script, it can be
	// between 1 and 4 bytes. So the min script length is 3 bytes smaller
	// than the maximum possible length.
	const minScriptLen = TaprootExpiryScriptSize - 3
	script := witness[1]
	if len(script) < minScriptLen || len(script) > TaprootExpiryScriptSize {
		return false
	}

	// The control block must start with the leaf version, which will always
	// be 0xc0 or 0xc1 in our case (depending on the parity of the key).
	const (
		leafVersionEven = byte(txscript.BaseLeafVersion)
		leafVersionOdd  = byte(txscript.BaseLeafVersion | 1)
	)
	if ctrlBlock[0] != leafVersionEven && ctrlBlock[0] != leafVersionOdd {
		return false
	}

	// The witness script should start with a 32-byte data push opcode and
	// end with CLTV. There's not much else we can assert about the script
	// since in this context we don't know the trader key or the actual
	// expiration.
	if script[0] != txscript.OP_DATA_32 ||
		script[len(script)-1] != txscript.OP_CHECKLOCKTIMEVERIFY {

		return false
	}
	return true
}

// hasAnnex returns true if the provided witness includes an annex element,
// otherwise returns false.
func hasAnnex(witness wire.TxWitness) bool {
	// By definition, the annex element can not be the sole element in the
	// witness stack.
	if len(witness) < 2 {
		return false
	}

	// If an annex element is included in the witness stack, by definition,
	// it will be the last element and will be prefixed by a Taproot annex
	// tag.
	lastElement := witness[len(witness)-1]
	if len(lastElement) == 0 {
		return false
	}

	return lastElement[0] == txscript.TaprootAnnexTag
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

// IsTaprootMultiSigSpend determines whether the provided witness corresponds to
// the MuSig2 multi-sig key spend path of a Taproot enabled (version 1) account.
func IsTaprootMultiSigSpend(witness wire.TxWitness) bool {
	if len(witness) != 1 {
		return false
	}

	// We'll always use SigHashDefault, so our signature will always be 64
	// bytes long exactly.
	if len(witness[0]) != schnorr.SignatureSize {
		return false
	}
	return true
}

// TaprootMuSig2SigningSession creates a MuSig2 signing session for a Taproot
// account spend.
func TaprootMuSig2SigningSession(ctx context.Context, version Version,
	expiry uint32, traderKey, batchKey *btcec.PublicKey,
	sharedSecret [32]byte, auctioneerKey *btcec.PublicKey,
	signer lndclient.SignerClient, localKeyLocator *keychain.KeyLocator,
	remoteNonces *MuSig2Nonces) (*input.MuSig2SessionInfo, func(), error) {

	expiryLeaf, err := TaprootExpiryScript(
		expiry, traderKey, batchKey, sharedSecret,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating expiry leaf "+
			"script: %v", err)
	}

	rootHash := expiryLeaf.TapHash()
	sessionOpts := []lndclient.MuSig2SessionOpts{
		lndclient.MuSig2TaprootTweakOpt(rootHash[:], false),
	}

	var remoteNonceBytes []byte
	if remoteNonces != nil {
		remoteNonceBytes = remoteNonces[:]
		sessionOpts = append(sessionOpts, lndclient.MuSig2NonceOpt(
			[][musig2.PubNonceSize]byte{*remoteNonces},
		))
	}

	var (
		signers       [][]byte
		muSig2Version input.MuSig2Version
	)

	switch version {
	case VersionTaprootMuSig2:
		muSig2Version = input.MuSig2Version040
		signers = [][]byte{
			schnorr.SerializePubKey(traderKey),
			schnorr.SerializePubKey(auctioneerKey),
		}

	case VersionTaprootMuSig2V100RC2:
		muSig2Version = input.MuSig2Version100RC2
		signers = [][]byte{
			traderKey.SerializeCompressed(),
			auctioneerKey.SerializeCompressed(),
		}

	default:
		return nil, nil, fmt.Errorf("unsupported version: %v", version)
	}

	sessionInfo, err := signer.MuSig2CreateSession(
		ctx, muSig2Version, localKeyLocator, signers, sessionOpts...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating MuSig2 session: %v",
			err)
	}

	log.Tracef("Created MuSig2 signing session version=%d for expiry=%d, "+
		"traderKey=%x, batchKey=%x, sharedSecret=%x, "+
		"auctioneerKey=%x, rootHash=%x, combinedKey=%x, "+
		"localNonces=%x, remoteNonces=%x", version,
		expiry, traderKey.SerializeCompressed(),
		batchKey.SerializeCompressed(), sharedSecret[:],
		auctioneerKey.SerializeCompressed(), rootHash[:],
		sessionInfo.CombinedKey.SerializeCompressed(),
		sessionInfo.PublicNonce[:], remoteNonceBytes)

	return sessionInfo, func() {
		err = signer.MuSig2Cleanup(ctx, sessionInfo.SessionID)
		if err != nil {
			log.Errorf("Error cleaning up MuSig2 session %x: %v",
				sessionInfo.SessionID[:], err)
		}
	}, nil
}

// TaprootMuSig2Sign creates a partial MuSig2 signature for a Taproot account
// spend. If remoteSigs is not empty, we expect to be the second (and last)
// signer and will also attempt to combine the signatures. The return value in
// that case is the full, final signature instead of the partial signature.
func TaprootMuSig2Sign(ctx context.Context, inputIdx int,
	sessionInfo *input.MuSig2SessionInfo, signer lndclient.SignerClient,
	spendTx *wire.MsgTx, previousOutputs []*wire.TxOut,
	remoteNonces *MuSig2Nonces,
	remotePartialSig *[input.MuSig2PartialSigSize]byte) ([]byte, error) {

	// In some cases we can already register all nonces during session
	// creation.
	var remoteNonceBytes []byte
	if remoteNonces != nil {
		remoteNonceBytes = remoteNonces[:]
		allNonces, err := signer.MuSig2RegisterNonces(
			ctx, sessionInfo.SessionID,
			[][musig2.PubNonceSize]byte{*remoteNonces},
		)
		if err != nil {
			return nil, fmt.Errorf("error registering remote "+
				"nonces: %v", err)
		}
		if !allNonces {
			return nil, fmt.Errorf("don't have all nonces after " +
				"registering remote nonces")
		}
	}

	// We now need to create the raw sighash of the transaction, as that
	// will be the message we're signing collaboratively.
	prevOutputFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx := range spendTx.TxIn {
		prevOutputFetcher.AddPrevOut(
			spendTx.TxIn[idx].PreviousOutPoint,
			previousOutputs[idx],
		)
	}
	sighashes := txscript.NewTxSigHashes(spendTx, prevOutputFetcher)

	sigHash, err := txscript.CalcTaprootSignatureHash(
		sighashes, txscript.SigHashDefault, spendTx, inputIdx,
		prevOutputFetcher,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating sighash: %v", err)
	}

	// We'll attempt to combine our signature with the remote one if there
	// is one. In that case we won't clean up after signing, since we still
	// need the session for the combine step.
	shouldCombine := remotePartialSig != nil

	var digest [32]byte
	copy(digest[:], sigHash)
	partialSig, err := signer.MuSig2Sign(
		ctx, sessionInfo.SessionID, digest, !shouldCombine,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating partial signature: %v",
			err)
	}

	log.Tracef("Signed MuSig2 sighash=%x for taprootKey=%x, "+
		"partialSig=%x, remoteNonce=%x", sigHash,
		schnorr.SerializePubKey(sessionInfo.CombinedKey),
		partialSig, remoteNonceBytes)

	// If we're not the last signer, we're done now and the session should
	// have been cleaned up. We return our partial signature, the remote
	// party will do the combine step.
	if !shouldCombine {
		return partialSig, nil
	}

	// We have the remote signature, so we're the last signer.
	log.Tracef("Combining ourPartialSig=%x with remotePartialSig=%x",
		partialSig, remotePartialSig[:])
	haveFinalSig, finalSig, err := signer.MuSig2CombineSig(
		ctx, sessionInfo.SessionID, [][]byte{remotePartialSig[:]},
	)
	if err != nil {
		return nil, fmt.Errorf("error combining sigs: %v", err)
	}

	// We should now have a full, complete signature. If this is true then
	// the signer also automatically cleaned up the signing session, so we
	// don't need to do anything.
	if !haveFinalSig {
		return nil, fmt.Errorf("don't have final sig after combining " +
			"remote partial signature with ours")
	}

	return finalSig, nil
}

// IncrementKey increments the given key by the backing curve's base point.
func IncrementKey(pubKey *btcec.PublicKey) *btcec.PublicKey {
	var (
		key, g, res btcec.JacobianPoint
	)
	pubKey.AsJacobian(&key)

	// Multiply G by 1 to get G.
	secp.ScalarBaseMultNonConst(new(secp.ModNScalar).SetInt(1), &g)

	secp.AddNonConst(&key, &g, &res)
	res.ToAffine()
	return btcec.NewPublicKey(&res.X, &res.Y)
}

// DecrementKey is the opposite of IncrementKey, it "subtracts one" from the
// current key to arrive at the key used before the IncrementKey operation.
func DecrementKey(pubKey *btcec.PublicKey) *btcec.PublicKey {
	var (
		key, g, res btcec.JacobianPoint
	)
	pubKey.AsJacobian(&key)

	// Multiply G by 1 to get G.
	secp.ScalarBaseMultNonConst(new(secp.ModNScalar).SetInt(1), &g)

	// Get -G by negating the Y axis. We normalize first, so we can negate
	// with the magnitude of 1 and then again to make sure everything is
	// normalized again after the negation.
	g.ToAffine()
	g.Y.Normalize()
	g.Y.Negate(1)
	g.Y.Normalize()

	//  priorKey = key - G
	//  priorKey = (key.x, key.y) + (G.x, -G.y)
	secp.AddNonConst(&key, &g, &res)

	res.ToAffine()
	return btcec.NewPublicKey(&res.X, &res.Y)
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
// in any of the provided transactions.
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
