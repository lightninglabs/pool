package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
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
	case bool:
		return lnwire.WriteBool(w, e)

	case uint8:
		return lnwire.WriteUint8(w, e)

	case uint32:
		return lnwire.WriteUint32(w, e)

	case uint64:
		return lnwire.WriteUint64(w, e)

	case btcutil.Amount:
		return lnwire.WriteSatoshi(w, e)

	case chainfee.SatPerKWeight:
		return lnwire.WriteUint64(w, uint64(e))

	case *keychain.KeyDescriptor:
		return WriteElements(w, e.KeyLocator, e.PubKey)

	case keychain.KeyLocator:
		if err := binary.Write(w, byteOrder, e.Family); err != nil {
			return err
		}
		return binary.Write(w, byteOrder, e.Index)

	case *btcec.PublicKey:
		return lnwire.WritePublicKey(w, e)

	case lntypes.Preimage:
		return lnwire.WriteBytes(w, e[:])

	case []byte:
		return lnwire.WriteBytes(w, e)

	case [32]byte:
		return lnwire.WriteBytes(w, e[:])

	case [33]byte:
		return lnwire.WriteBytes(w, e[:])

	case time.Time:
		return lnwire.WriteUint64(w, uint64(e.UnixNano()))

	case *wire.MsgTx:
		return e.Serialize(w)

	case wire.OutPoint:
		return lnwire.WriteOutPoint(w, e)

	case []net.Addr:
		return lnwire.WriteNetAddrs(w, e)

	default:
		return fmt.Errorf("unhandled element type: %T", element)
	}
}
