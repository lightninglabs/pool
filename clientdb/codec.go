package clientdb

import (
	"encoding/binary"
	"io"

	"github.com/lightninglabs/agora/client/account"
	"github.com/lightningnetwork/lnd/keychain"
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
		if err := lnwire.WriteElement(w, uint8(e)); err != nil {
			return err
		}

	case keychain.KeyLocator:
		if err := binary.Write(w, byteOrder, e.Family); err != nil {
			return err
		}
		if err := binary.Write(w, byteOrder, e.Index); err != nil {
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

	case *keychain.KeyLocator:
		if err := binary.Read(r, byteOrder, &e.Family); err != nil {
			return err
		}
		if err := binary.Read(r, byteOrder, &e.Index); err != nil {
			return err
		}

	default:
		return lnwire.ReadElement(r, element)
	}

	return nil
}
