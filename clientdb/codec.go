package clientdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/pool/account"
	"github.com/lightninglabs/pool/codec"
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
	case *order.NodeTier:
		var v uint32
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = order.NodeTier(v)

	case *account.State:
		var s uint8
		if err := lnwire.ReadElement(r, &s); err != nil {
			return err
		}
		*e = account.State(s)

	case *account.Version:
		var v uint8
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = account.Version(v)

	case *order.Version:
		var v uint32
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = order.Version(v)

	case *order.BatchVersion:
		var v uint32
		if err := lnwire.ReadElement(r, &v); err != nil {
			return err
		}
		*e = order.BatchVersion(v)

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

	case *time.Time:
		var ns uint64
		if err := lnwire.ReadElement(r, &ns); err != nil {
			return err
		}
		*e = time.Unix(0, int64(ns))

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
	if err := codec.WriteElement(&valueBuf, value); err != nil {
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
		if err := codec.WriteElement(&buf, defaultValue); err != nil {
			return err
		}
		rawValue = buf.Bytes()
	}

	// Now that the raw value contains a value for sure, use the generic
	// read function we use for all other fields.
	return ReadElement(bytes.NewReader(rawValue), valueTarget)
}
