package codec

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	byteOrder = binary.BigEndian
)

// WriteElements writes each element in the elements slice to the passed buffer
// using WriteElement.
func WriteElements(w *bytes.Buffer, elements ...interface{}) error {
	for _, element := range elements {
		err := WriteElement(w, element)
		if err != nil {
			return err
		}
	}
	return nil
}

// WriteElement is a one-stop shop to write the big endian representation of
// any element which is to be serialized.
func WriteElement(w *bytes.Buffer, element interface{}) error {
	switch e := element.(type) {
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

	case time.Time:
		return lnwire.WriteElement(w, uint64(e.UnixNano()))

	case *wire.MsgTx:
		if err := e.Serialize(w); err != nil {
			return err
		}

	default:
		return lnwire.WriteElement(w, element)
	}

	return nil
}
