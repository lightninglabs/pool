package sidecar

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	idType      tlv.Type = 1
	versionType tlv.Type = 2
	stateType   tlv.Type = 3

	offerType              tlv.Type = 10
	capacityType           tlv.Type = 11
	pushAmtType            tlv.Type = 12
	leaseDurationType      tlv.Type = 13
	signPubKeyType         tlv.Type = 14
	sigOfferDigestType     tlv.Type = 15
	offerAutoType          tlv.Type = 16
	unannouncedChannelType tlv.Type = 17
	zeroConfChannelType    tlv.Type = 18

	recipientType        tlv.Type = 20
	nodePubKeyType       tlv.Type = 21
	multiSigPubKeyType   tlv.Type = 22
	multiSigKeyIndexType tlv.Type = 23

	orderType          tlv.Type = 30
	bidNonceType       tlv.Type = 31
	sigOrderDigestType tlv.Type = 32

	executionType        tlv.Type = 40
	pendingChannelIDType tlv.Type = 41
)

var (
	// ZeroSignature is an empty signature with all bits set to zero.
	ZeroSignature = [64]byte{}
)

// SerializeTicket binary serializes the given ticket to the writer using the
// tlv format.
func SerializeTicket(w io.Writer, ticket *Ticket) error {
	if ticket == nil {
		return fmt.Errorf("ticket cannot be nil")
	}

	var (
		version    = uint8(ticket.Version)
		state      = uint8(ticket.State)
		offerBytes []byte
		err        error
	)

	offerBytes, err = serializeOffer(ticket.Offer)
	if err != nil {
		return err
	}

	tlvRecords := []tlv.Record{
		tlv.MakeStaticRecord(idType, &ticket.ID, 8, EBytes8, DBytes8),
		tlv.MakePrimitiveRecord(versionType, &version),
		tlv.MakePrimitiveRecord(stateType, &state),
		tlv.MakePrimitiveRecord(offerType, &offerBytes),
	}

	if ticket.Recipient != nil {
		recipientBytes, err := serializeRecipient(*ticket.Recipient)
		if err != nil {
			return err
		}
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			recipientType, &recipientBytes,
		))
	}

	if ticket.Order != nil {
		orderBytes, err := serializeOrder(*ticket.Order)
		if err != nil {
			return err
		}
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			orderType, &orderBytes,
		))
	}

	if ticket.Execution != nil {
		executionBytes, err := serializeExecution(*ticket.Execution)
		if err != nil {
			return err
		}
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			executionType, &executionBytes,
		))
	}

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// DeserializeTicket deserializes a ticket from the given reader, expecting the
// data to be encoded in the tlv format.
func DeserializeTicket(r io.Reader) (*Ticket, error) {
	var (
		ticket                     = &Ticket{}
		version, state             uint8
		offerBytes, recipientBytes []byte
		orderBytes, executionBytes []byte
	)

	tlvStream, err := tlv.NewStream(
		tlv.MakeStaticRecord(idType, &ticket.ID, 8, EBytes8, DBytes8),
		tlv.MakePrimitiveRecord(versionType, &version),
		tlv.MakePrimitiveRecord(stateType, &state),
		tlv.MakePrimitiveRecord(offerType, &offerBytes),
		tlv.MakePrimitiveRecord(recipientType, &recipientBytes),
		tlv.MakePrimitiveRecord(orderType, &orderBytes),
		tlv.MakePrimitiveRecord(executionType, &executionBytes),
	)
	if err != nil {
		return nil, err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return nil, err
	}

	ticket.Version = Version(version)
	ticket.State = State(state)

	if t, ok := parsedTypes[offerType]; ok && t == nil {
		ticket.Offer, err = deserializeOffer(offerBytes)
		if err != nil {
			return nil, err
		}
	}

	if t, ok := parsedTypes[recipientType]; ok && t == nil {
		recipient, err := deserializeRecipient(recipientBytes)
		if err != nil {
			return nil, err
		}
		ticket.Recipient = &recipient
	}

	if t, ok := parsedTypes[orderType]; ok && t == nil {
		order, err := deserializeOrder(orderBytes)
		if err != nil {
			return nil, err
		}
		ticket.Order = &order
	}

	if t, ok := parsedTypes[executionType]; ok && t == nil {
		execution, err := deserializeExecution(executionBytes)
		if err != nil {
			return nil, err
		}
		ticket.Execution = &execution
	}

	return ticket, nil
}

// serializeOffer serializes the given offer to a byte slice using the tlv
// format.
func serializeOffer(o Offer) ([]byte, error) {
	capacity := uint64(o.Capacity)
	pushAmt := uint64(o.PushAmt)

	var autoAsInt uint8
	if o.Auto {
		autoAsInt = 1
	}

	tlvRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(capacityType, &capacity),
		tlv.MakePrimitiveRecord(pushAmtType, &pushAmt),
		tlv.MakePrimitiveRecord(
			leaseDurationType, &o.LeaseDurationBlocks,
		),
		tlv.MakePrimitiveRecord(offerAutoType, &autoAsInt),
	}

	if o.UnannouncedChannel {
		isUnannounced := uint8(1)
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			unannouncedChannelType, &isUnannounced,
		))
	}

	if o.ZeroConfChannel {
		isZeroConf := uint8(1)
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			zeroConfChannelType, &isZeroConf,
		))
	}

	if o.SignPubKey != nil {
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			signPubKeyType, &o.SignPubKey,
		))
	}

	if o.SigOfferDigest != nil {
		tlvRecords = append(tlvRecords, tlv.MakeStaticRecord(
			sigOfferDigestType, &o.SigOfferDigest, 64, ESig, DSig,
		))
	}

	return encodeBytes(tlvRecords...)
}

// deserializeOffer deserializes an order from the given byte slice, expecting
// the data to be encoded in the tlv format.
func deserializeOffer(offerBytes []byte) (Offer, error) {
	var (
		o                 = Offer{}
		capacity, pushAmt uint64
		autoAsInt         uint8
		isUnannounced     uint8
		isZeroConf        uint8
	)

	if err := decodeBytes(
		offerBytes,
		tlv.MakePrimitiveRecord(capacityType, &capacity),
		tlv.MakePrimitiveRecord(pushAmtType, &pushAmt),
		tlv.MakePrimitiveRecord(
			leaseDurationType, &o.LeaseDurationBlocks,
		),
		tlv.MakePrimitiveRecord(signPubKeyType, &o.SignPubKey),
		tlv.MakeStaticRecord(
			sigOfferDigestType, &o.SigOfferDigest, 64, ESig, DSig,
		),
		tlv.MakePrimitiveRecord(offerAutoType, &autoAsInt),
		tlv.MakePrimitiveRecord(unannouncedChannelType, &isUnannounced),
		tlv.MakePrimitiveRecord(zeroConfChannelType, &isZeroConf),
	); err != nil {
		return o, err
	}

	o.Auto = autoAsInt == 1
	o.UnannouncedChannel = isUnannounced == 1
	o.ZeroConfChannel = isZeroConf == 1
	o.Capacity = btcutil.Amount(capacity)
	o.PushAmt = btcutil.Amount(pushAmt)

	return o, nil
}

// serializeRecipient serializes the given recipient to a byte slice using the
// tlv format.
func serializeRecipient(r Recipient) ([]byte, error) {
	var tlvRecords []tlv.Record
	if r.NodePubKey != nil {
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			nodePubKeyType, &r.NodePubKey,
		))
	}

	if r.MultiSigPubKey != nil {
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			multiSigPubKeyType, &r.MultiSigPubKey,
		))
	}

	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		multiSigKeyIndexType, &r.MultiSigKeyIndex,
	))

	return encodeBytes(tlvRecords...)
}

// deserializeRecipient deserializes a recipient from the given byte slice,
// expecting the data to be encoded in the tlv format.
func deserializeRecipient(recipientBytes []byte) (Recipient, error) {
	r := Recipient{}
	return r, decodeBytes(
		recipientBytes,
		tlv.MakePrimitiveRecord(nodePubKeyType, &r.NodePubKey),
		tlv.MakePrimitiveRecord(multiSigPubKeyType, &r.MultiSigPubKey),
		tlv.MakePrimitiveRecord(
			multiSigKeyIndexType, &r.MultiSigKeyIndex,
		),
	)
}

// serializeOrder serializes the given order to a byte slice using the tlv
// format.
func serializeOrder(o Order) ([]byte, error) {
	tlvRecords := []tlv.Record{
		tlv.MakePrimitiveRecord(bidNonceType, &o.BidNonce),
	}

	if o.SigOrderDigest != nil {
		tlvRecords = append(tlvRecords, tlv.MakeStaticRecord(
			sigOrderDigestType, &o.SigOrderDigest, 64, ESig, DSig,
		))
	}

	return encodeBytes(tlvRecords...)
}

// deserializeOrder deserializes an order from the given byte slice, expecting
// the data to be encoded in the tlv format.
func deserializeOrder(orderBytes []byte) (Order, error) {
	o := Order{}
	return o, decodeBytes(
		orderBytes,
		tlv.MakePrimitiveRecord(bidNonceType, &o.BidNonce),
		tlv.MakeStaticRecord(
			sigOrderDigestType, &o.SigOrderDigest, 64, ESig, DSig,
		),
	)
}

// serializeExecution serializes the given execution to a byte slice using the
// tlv format.
func serializeExecution(e Execution) ([]byte, error) {
	return encodeBytes(tlv.MakePrimitiveRecord(
		pendingChannelIDType, &e.PendingChannelID,
	))
}

// deserializeExecution deserializes an execution from the given byte slice,
// expecting the data to be encoded in the tlv format.
func deserializeExecution(executionBytes []byte) (Execution, error) {
	e := Execution{}
	return e, decodeBytes(executionBytes, tlv.MakePrimitiveRecord(
		pendingChannelIDType, &e.PendingChannelID,
	))
}

// ESig is an Encoder for the btcec.Signature type. An error is returned if val
// is not a **btcec.Signature.
func ESig(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(**ecdsa.Signature); ok {
		var s [64]byte
		if *v != nil {
			sig, err := lnwire.NewSigFromSignature(*v)
			if err != nil {
				return err
			}
			copy(s[:], sig.RawBytes())
		}

		return tlv.EBytes64(w, &s, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "*btcec.Signature")
}

// DSig is a Decoder for the btcec.Signature type. An error is returned if val
// is not a **btcec.Signature.
func DSig(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(**ecdsa.Signature); ok && l == 64 {
		var s [64]byte
		if err := tlv.DBytes64(r, &s, buf, 64); err != nil {
			return err
		}

		if s != ZeroSignature {
			wireSig, err := lnwire.NewSigFromWireECDSA(s[:])
			if err != nil {
				return err
			}
			ecSig, err := wireSig.ToSignature()
			if err != nil {
				return err
			}

			*v = ecSig.(*ecdsa.Signature)
		}

		return nil
	}
	return tlv.NewTypeForEncodingErr(val, "*btcec.Signature")
}

// EBytes8 is an Encoder for 8-byte arrays. An error is returned if val is not
// a *[8]byte.
func EBytes8(w io.Writer, val interface{}, _ *[8]byte) error {
	if b, ok := val.(*[8]byte); ok {
		_, err := w.Write(b[:])
		return err
	}
	return tlv.NewTypeForEncodingErr(val, "[8]byte")
}

// DBytes8 is a Decoder for 8-byte arrays. An error is returned if val is not
// a *[8]byte.
func DBytes8(r io.Reader, val interface{}, _ *[8]byte, l uint64) error {
	if b, ok := val.(*[8]byte); ok && l == 8 {
		_, err := io.ReadFull(r, b[:])
		return err
	}
	return tlv.NewTypeForDecodingErr(val, "[8]byte", l, 8)
}

// encodeBytes encodes the given tlv records into a byte slice.
func encodeBytes(tlvRecords ...tlv.Record) ([]byte, error) {
	tlv.SortRecords(tlvRecords)

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := tlvStream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// decodeBytes decodes the given byte slice interpreting the data as a tlv
// stream containing the given records.
func decodeBytes(tlvBytes []byte, tlvRecords ...tlv.Record) error {
	tlv.SortRecords(tlvRecords)

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Decode(bytes.NewReader(tlvBytes))
}
