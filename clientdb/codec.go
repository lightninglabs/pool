package clientdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/terms"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"go.etcd.io/bbolt"
)

var (
	// additionalDataSuffix is the suffix of the sub-bucket we are going to
	// use to store new/additional data of objects in. The bucket structure
	// will look this way with the example of the accounts bucket:
	// [account]:                          root account bucket
	//   - <acct_key_1>:                   <binary serialized base data>
	//   - <acct_key_2>:                   <binary serialized base data>
	//   [<acct_key_1>-additional-data]:   additional data bucket
	//     - funding-conf-target:          <binary serialized value>
	additionalDataSuffix = []byte("-additional-data")
)

// WriteElements is writes each element in the elements slice to the passed
// io.Writer using WriteElement.
func WriteElements(w io.Writer, elements ...interface{}) error {
	for _, element := range elements {
		err := WriteElement(w, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized. The passed io.Writer should be backed
// by an appropriately sized byte slice, or be able to dynamically expand to
// accommodate additional data.
func WriteElement(w io.Writer, element interface{}) error {
	switch e := element.(type) {
	case account.State:
		return lnwire.WriteElement(w, uint8(e))

	case order.Version:
		return lnwire.WriteElement(w, uint32(e))

	case order.Type:
		return lnwire.WriteElement(w, uint8(e))

	case order.State:
		return lnwire.WriteElement(w, uint8(e))

	case order.MatchState:
		return lnwire.WriteElement(w, uint8(e))

	case order.SupplyUnit:
		return lnwire.WriteElement(w, uint64(e))

	case order.FixedRatePremium:
		return lnwire.WriteElement(w, uint32(e))

	case order.Nonce:
		return lnwire.WriteElement(w, e[:])

	case terms.LinearFeeSchedule:
		return lnwire.WriteElements(
			w, uint64(e.BaseFee()), uint64(e.FeeRate()),
		)

	case chainfee.SatPerKWeight:
		return lnwire.WriteElement(w, uint64(e))

	case *keychain.KeyDescriptor:
		if err := WriteElements(w, e.KeyLocator, e.PubKey); err != nil {
			return err
		}

	case keychain.KeyLocator:
		if err := binary.Write(w, byteOrder, e.Family); err != nil {
			return err
		}
		if err := binary.Write(w, byteOrder, e.Index); err != nil {
			return err
		}

	case lntypes.Preimage:
		return lnwire.WriteElement(w, e[:])

	case [32]byte:
		return lnwire.WriteElement(w, e[:])

	case *wire.MsgTx:
		if err := e.Serialize(w); err != nil {
			return err
		}

	default:
		return lnwire.WriteElement(w, element)
	}

	return nil
}

// ReadElements deserializes a variable number of elements into the passed
// io.Reader, with each element being deserialized according to the ReadElement
// function.
func ReadElements(r io.Reader, elements ...interface{}) error {
	for _, element := range elements {
		err := ReadElement(r, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// ReadElement is a one-stop utility function to deserialize any data structure.
func ReadElement(r io.Reader, element interface{}) error { // nolint:gocyclo
	switch e := element.(type) {
	case *account.State:
		var s uint8
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = account.State(s)

	case *order.Version:
		var v uint32
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = order.Version(v)

	case *order.Type:
		var v uint8
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = order.Type(v)

	case *order.State:
		var s uint8
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = order.State(s)

	case *order.MatchState:
		var s uint8
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = order.MatchState(s)

	case *order.SupplyUnit:
		var s uint64
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = order.SupplyUnit(s)

	case *order.FixedRatePremium:
		var s uint32
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = order.FixedRatePremium(s)

	case *order.Nonce:
		if err := lnwire.ReadElement(r, e[:]); err != nil {
			return err
		}

	case *terms.LinearFeeSchedule:
		var base, rate uint64
		if err := lnwire.ReadElements(r, &base, &rate); err != nil {
			return err
		}

		*e = *terms.NewLinearFeeSchedule(
			btcutil.Amount(base), btcutil.Amount(rate),
		)

	case *chainfee.SatPerKWeight:
		var v uint64
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = chainfee.SatPerKWeight(v)

	case **keychain.KeyDescriptor:
		var keyDesc keychain.KeyDescriptor
		err := ReadElements(r, &keyDesc.KeyLocator, &keyDesc.PubKey)
		if err != nil {
			return err
		}
		*e = &keyDesc

	case *keychain.KeyLocator:
		if err := binary.Read(r, byteOrder, &e.Family); err != nil {
			return err
		}
		if err := binary.Read(r, byteOrder, &e.Index); err != nil {
			return err
		}

	case *lntypes.Preimage:
		if err := lnwire.ReadElement(r, e[:]); err != nil {
			return err
		}

	case *[32]byte:
		if err := lnwire.ReadElement(r, e[:]); err != nil {
			return err
		}

	case **wire.MsgTx:
		var tx wire.MsgTx
		if err := tx.Deserialize(r); err != nil {
			return err
		}
		*e = &tx

	default:
		return lnwire.ReadElement(r, element)
	}

	return nil
}

// getAdditionalDataBucket returns the bucket for additional data of a database
// object located at mainKey.
func getAdditionalDataBucket(parentBucket *bbolt.Bucket,
	mainKey []byte, createBucket bool) (*bbolt.Bucket, error) {

	keyLen := len(mainKey) + len(additionalDataSuffix)
	additionalDataKey := make([]byte, keyLen)
	copy(additionalDataKey, mainKey)
	copy(additionalDataKey[len(mainKey):], additionalDataSuffix)

	// When only reading from the DB we don't want to create the bucket if
	// it doesn't exist as that would fail on the read-only TX anyway.
	if createBucket {
		return parentBucket.CreateBucketIfNotExists(additionalDataKey)
	}

	return parentBucket.Bucket(additionalDataKey), nil
}

func writeAdditionalValue(targetBucket *bbolt.Bucket, valueKey []byte,
	value interface{}) error {

	var valueBuf bytes.Buffer
	if err := WriteElement(&valueBuf, value); err != nil {
		return err
	}

	return targetBucket.Put(valueKey, valueBuf.Bytes())
}

func readAdditionalValue(sourceBucket *bbolt.Bucket, valueKey []byte,
	valueTarget interface{}, defaultValue interface{}) error {

	// First of all, check that the valueTarget is a pointer to the same
	// type as the defaultValue. Otherwise our fallback hack below won't
	// work.
	defaultValueType := reflect.TypeOf(defaultValue)
	valueTargetType := reflect.TypeOf(valueTarget)
	if valueTargetType != reflect.PtrTo(defaultValueType) {
		return fmt.Errorf("the defaultValue must be of the same type "+
			"as valueTarget is a pointer of, got %v expected %v",
			defaultValueType, valueTargetType.Elem())
	}

	var rawValue []byte

	// If the additional values bucket exists, see if we have a value for
	// this value key. If either the bucket and/or the value doesn't exist,
	// we'll fall back to the default value below.
	if sourceBucket != nil {
		rawValue = sourceBucket.Get(valueKey)
	}

	if rawValue == nil {
		// No value is stored for this field yet, so we set the default
		// value. We trick a bit here so we don't have to care about the
		// data type. We just serialize the default value (which _MUST_
		// be of the same data type as the target value is a pointer of)
		// with our generic write function, then pass that raw value to
		// the generic read function.
		var buf bytes.Buffer
		if err := WriteElement(&buf, defaultValue); err != nil {
			return err
		}
		rawValue = buf.Bytes()
	}

	// Now that the raw value contains a value for sure, use the generic
	// read function we use for all other fields.
	return ReadElement(bytes.NewReader(rawValue), valueTarget)
}
