package clientdb

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/agora/client/account"
	"github.com/lightninglabs/agora/client/order"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
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

	case order.SupplyUnit:
		return lnwire.WriteElement(w, uint64(e))

	case order.FixedRatePremium:
		return lnwire.WriteElement(w, uint32(e))

	case order.Nonce:
		return lnwire.WriteElement(w, e[:])

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
func ReadElement(r io.Reader, element interface{}) error {
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
