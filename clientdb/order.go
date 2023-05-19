package clientdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/pool/codec"
	"github.com/lightninglabs/pool/event"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"github.com/lightningnetwork/lnd/tlv"
	"go.etcd.io/bbolt"
)

const (
	// bidSelfChanBalanceType is the tlv type we use to store the self
	// channel balance on bid orders.
	bidSelfChanBalanceType tlv.Type = 1

	// bidSidecarTicketType is the tlv type we use to store the sidecar
	// ticket on bid orders.
	bidSidecarTicketType tlv.Type = 2

	// orderChannelType is the tlv type we use to store the desired channel
	// type resulting from a matched order.
	orderChannelType tlv.Type = 3

	// allowedNodeIDsType is the tlv type we use to store the list of node
	// ids the order is allowed to match with.
	allowedNodeIDsType tlv.Type = 4

	// notAllowedNodeIDsType is the tlv type we use to store the list of
	// node ids the order is not allowed to match with.
	notAllowedNodeIDsType tlv.Type = 5

	// bidUnannouncedChannelType is the tlv type we use to store a flag
	// value when a bid requires unannounced channels.
	bidUnannouncedChannelType tlv.Type = 6

	// askChannelAnnouncementConstraintsType is the tlv type that we use
	// to store the channel announcement match preferences.
	askChannelAnnouncementConstraintsType tlv.Type = 7

	// bidZeroConfType is the tlv type we use to store a flag value when
	// a bid asks for zero conf channels.
	bidZeroConfType tlv.Type = 8

	// askChannelConfirmationConstraintsType is the tlv type that we use
	// to store the channel confirmation match preferences.
	askChannelConfirmationConstraintsType tlv.Type = 9

	// orderAuctionType is the tlv type that we use to store the auction
	// type for an order.
	orderAuctionType tlv.Type = 10

	// orderIsPublicType is the tlv type we use to store a flag value when
	// it is ok to share details of this order in public markets.
	orderIsPublicType tlv.Type = 11
)

var (
	// ErrNoOrder is the error returned if no order with the given nonce
	// exists in the store.
	ErrNoOrder = errors.New("no order found")

	// ErrOrderExists is returned if an order is submitted that is already
	// known to the store.
	ErrOrderExists = errors.New("order with this nonce already exists")

	// ordersBucketKey is a bucket that contains all orders that are
	// currently pending or completed. This bucket is keyed by the nonce and
	// leads to a nested sub-bucket that houses information for that order.
	ordersBucketKey = []byte("orders")

	// orderKey is the key that stores the serialized order. It is nested
	// within the sub-bucket for each active order.
	//
	// path: ordersBucketKey -> orderBucket[nonce] -> orderKey
	orderKey = []byte("order")

	// orderTierKey is a key within the order bucket for an order. It
	// stores the min node tier for an order. It allows an order to specify
	// the required minimum node tier for matching and execution.
	orderTierKey = []byte("order-tier")

	// orderMinUnitsMatchKey is the key that stores the serialized order's
	// minimum units match. It is nested within the sub-bucket for each
	// active order.
	//
	// path: ordersBucketKey -> orderBucket[nonce] -> orderMinUnitsMatchKey
	orderMinUnitsMatchKey = []byte("order-min-units-match")

	// orderTlvKey is a key within the order bucket for additional, tlv
	// encoded data.
	orderTlvKey = []byte("order-tlv")
)

type extraOrderData struct {
	minNodeTier   order.NodeTier
	minUnitsMatch order.SupplyUnit
	tlvData       []byte
}

// orderCallback is a function type that is used to pass as a callback into the
// store's fetch functions to deliver the results to the caller. It also
// includes any auxiliary order information as additional arguments after the
// raw order bytes.
type orderCallback func(nonce order.Nonce, rawOrder []byte,
	extraData *extraOrderData) error

// SubmitOrder stores an order by using the orders's nonce as an identifier. If
// an order with the given nonce already exists in the store, ErrOrderExists is
// returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) SubmitOrder(newOrder order.Order) error {
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		err = fetchOrderTX(rootBucket, newOrder.Nonce(), nil)
		switch err {
		// No error means there is an order with that nonce in the DB
		// already.
		case nil:
			return ErrOrderExists

		// This is what we want, no order with that nonce should be
		// known.
		case ErrNoOrder:

		// Surface any other error.
		default:
			return err
		}

		// Serialize and store the order now that we know it doesn't
		// exist yet.
		var w bytes.Buffer
		err = SerializeOrder(newOrder, &w)
		if err != nil {
			return err
		}

		// Create an event for the initial order and store it in the
		// order's event bucket but also in the global events under an
		// unique timestamp.
		evt := NewCreatedEvent(newOrder)
		err = storeOrderTX(
			rootBucket, newOrder.Nonce(), w.Bytes(), evt,
		)
		if err != nil {
			return err
		}
		err = storeOrderMinUnitsMatchTX(
			rootBucket, newOrder.Nonce(),
			newOrder.Details().MinUnitsMatch,
		)
		if err != nil {
			return err
		}

		// Next, store any additional order data as a tlv stream.
		if err := storeOrderTlvTX(
			rootBucket, newOrder.Nonce(), newOrder,
		); err != nil {
			return err
		}

		// Finally, store the min node tier, but only if this is a Bid
		// order.
		if bidOrder, ok := newOrder.(*order.Bid); ok {
			return storeOrderMinNoderTierTX(
				rootBucket, newOrder.Nonce(),
				bidOrder.MinNodeTier,
			)
		}

		return nil
	})
}

// UpdateOrder updates an order in the database according to the given
// modifiers.
//
// NOTE: This is part of the Store interface.
func (db *DB) UpdateOrder(nonce order.Nonce, modifiers ...order.Modifier) error {
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		_, err = updateOrder(rootBucket, rootBucket, nonce, modifiers)
		return err
	})
}

// UpdateOrders atomically updates a list of orders in the database according to
// the given modifiers.
//
// NOTE: This is part of the Store interface.
func (db *DB) UpdateOrders(nonces []order.Nonce,
	modifiers [][]order.Modifier) error {

	if len(nonces) != len(modifiers) {
		return fmt.Errorf("invalid number of modifiers")
	}

	// Read and update the orders in one single transaction that they are
	// updated atomically.
	return db.Update(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		for idx, nonce := range nonces {
			_, err := updateOrder(
				rootBucket, rootBucket, nonce, modifiers[idx],
			)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// GetOrder returns an order by looking up the nonce. If no order with that
// nonce exists in the store, ErrNoOrder is returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) GetOrder(nonce order.Nonce) (order.Order, error) {
	var (
		o        order.Order
		err      error
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			tlvReader := bytes.NewReader(extraData.tlvData)
			err = deserializeOrderTlvData(tlvReader, o)
			if err != nil {
				return err
			}

			// TODO(roasbeef): factory func to de-dup w/ all other
			// instances?
			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			return nil
		}
	)
	err = db.View(func(tx *bbolt.Tx) error {
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}
		return fetchOrderTX(rootBucket, nonce, callback)
	})
	return o, err
}

// GetOrders returns all orders that are currently known to the store.
//
// NOTE: This is part of the Store interface.
func (db *DB) GetOrders() ([]order.Order, error) {
	var (
		orders   []order.Order
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err := DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			tlvReader := bytes.NewReader(extraData.tlvData)
			err = deserializeOrderTlvData(tlvReader, o)
			if err != nil {
				return err
			}

			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			orders = append(orders, o)
			return nil
		}
	)
	err := db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main order bucket key.
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}

		// We'll now traverse the root bucket for all orders. The
		// primary key is the order nonce itself.
		return rootBucket.ForEach(func(nonceBytes, val []byte) error {
			// Only go into things that we know are sub-bucket keys.
			if val != nil {
				return nil
			}

			// Get the order from the bucket and pass it to the
			// caller.
			var nonce order.Nonce
			copy(nonce[:], nonceBytes)
			return fetchOrderTX(rootBucket, nonce, callback)
		})
	})
	if err != nil {
		return nil, err
	}
	return orders, nil
}

// DeleteOrder removes the order with the given nonce. If no order with that
// nonce exists in the store, ErrNoOrder is returned.
//
// NOTE: This is part of the Store interface.
func (db *DB) DeleteOrder(nonce order.Nonce) error {
	return db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab our main order bucket key.
		rootBucket, err := getBucket(tx, ordersBucketKey)
		if err != nil {
			return err
		}

		// Check that the order exists in the main bucket.
		orderBucket := rootBucket.Bucket(nonce[:])
		if orderBucket == nil {
			return ErrNoOrder
		}

		return rootBucket.DeleteBucket(nonce[:])
	})
}

// storeOrderTX saves a byte serialized order in its specific sub bucket within
// the root orders bucket.
func storeOrderTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	orderBytes []byte, evt event.Event) error {

	// From the root bucket, we'll make a new sub order bucket using
	// the order nonce.
	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	// Add the event to the global event store but also add a reference to
	// it in the order bucket.
	if err := storeEventTX(orderBucket, evt); err != nil {
		return err
	}

	// With the order bucket created, we'll store the order itself.
	return orderBucket.Put(orderKey, orderBytes)
}

// storeOrderMinNoderTierTX saves the current node tier for a given order to
// the proper bucket.
func storeOrderMinNoderTierTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	minNodeTier order.NodeTier) error {

	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := codec.WriteElements(&b, uint32(minNodeTier)); err != nil {
		return err
	}

	return orderBucket.Put(orderTierKey, b.Bytes())
}

// storeOrderMinUnitsMatchTX saves the order's minimum units match within the
// order's root bucket.
func storeOrderMinUnitsMatchTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	minUnitsMatch order.SupplyUnit) error {

	// From the root bucket, we'll make a new sub order bucket using
	// the order nonce.
	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	// With the order bucket created, we'll store the order's min units
	// match.
	var buf bytes.Buffer
	err = codec.WriteElement(&buf, uint64(minUnitsMatch))
	if err != nil {
		return err
	}
	return orderBucket.Put(orderMinUnitsMatchKey, buf.Bytes())
}

// storeOrderTlvTX saves the order's additional information as a tlv stream
// within the order's root bucket.
func storeOrderTlvTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	o order.Order) error {

	orderBucket, err := rootBucket.CreateBucketIfNotExists(nonce[:])
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := serializeOrderTlvData(&b, o); err != nil {
		return err
	}

	return orderBucket.Put(orderTlvKey, b.Bytes())
}

// fetchOrderTX fetches the binary data of one order specified by its nonce from
// the root orders bucket.
func fetchOrderTX(rootBucket *bbolt.Bucket, nonce order.Nonce,
	callback orderCallback) error {

	// Get the main order bucket next.
	orderBucket := rootBucket.Bucket(nonce[:])
	if orderBucket == nil {
		return ErrNoOrder
	}

	// With the main order bucket obtained, we'll grab the raw order
	// bytes and pass them back to the caller.
	orderBytes := orderBucket.Get(orderKey)
	if orderBytes == nil {
		return fmt.Errorf("order bucket not found")
	}
	if callback == nil {
		return nil
	}

	// Try to read our extra order data. Use a default of 1 for the min
	// units matched if the field does not exist (possible for older
	// orders).
	extraData := &extraOrderData{
		minUnitsMatch: order.SupplyUnit(1),
		tlvData:       orderBucket.Get(orderTlvKey),
	}

	nodeTierBytes := orderBucket.Get(orderTierKey)
	if nodeTierBytes != nil {
		err := ReadElements(
			bytes.NewReader(nodeTierBytes), &extraData.minNodeTier,
		)
		if err != nil {
			return err
		}
	}

	minUnitsMatchBytes := orderBucket.Get(orderMinUnitsMatchKey)
	if minUnitsMatchBytes != nil {
		err := ReadElements(
			bytes.NewReader(minUnitsMatchBytes),
			&extraData.minUnitsMatch,
		)
		if err != nil {
			return err
		}
	}

	return callback(nonce, orderBytes, extraData)
}

// updateOrder fetches the binary data of one order specified by its nonce from
// the orders bucket, applies the modifiers, and stores it back into dst bucket.
// The dst bucket can be a different bucket than the orders bucket if an order
// update should be written to a staging area instead of being applied to the
// original order directly. Do not use this function for applying the staged
// changes to the final bucket but use copyOrder instead.
func updateOrder(ordersBucket, dst *bbolt.Bucket, nonce order.Nonce,
	modifiers []order.Modifier) (order.Order, error) {

	var (
		o        order.Order
		err      error
		callback = func(nonce order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			tlvReader := bytes.NewReader(extraData.tlvData)
			err = deserializeOrderTlvData(tlvReader, o)
			if err != nil {
				return err
			}

			if bidOrder, ok := o.(*order.Bid); ok {
				bidOrder.MinNodeTier = extraData.minNodeTier
			}
			o.Details().MinUnitsMatch = extraData.minUnitsMatch

			return nil
		}
	)

	// Retrieve the order stored in the database.
	err = fetchOrderTX(ordersBucket, nonce, callback)
	if err != nil {
		return nil, err
	}

	// Preserve the original state to detect a state change later.
	prevState := o.Details().State

	// Apply the given modifications to it and store it back.
	for _, modifier := range modifiers {
		modifier(o.Details())
	}
	var w bytes.Buffer
	err = SerializeOrder(o, &w)
	if err != nil {
		return nil, err
	}

	// Create an event for the update now. We always store the event
	// reference to the order in the main order bucket, even if the
	// destination bucket is the pending orders bucket.
	evt := NewUpdatedEvent(prevState, o)
	orderBucket := ordersBucket.Bucket(nonce[:])
	if orderBucket == nil {
		return nil, ErrNoOrder
	}

	// Add the event to the global event store but also add a reference to
	// it in the order's event reference sub bucket.
	if err := storeEventTX(orderBucket, evt); err != nil {
		return nil, err
	}

	// Store the order to the destination bucket now, skipping the event as
	// we've already written that to the source bucket.
	if err := storeOrderTX(dst, nonce, w.Bytes(), nil); err != nil {
		return nil, err
	}
	err = storeOrderMinUnitsMatchTX(dst, nonce, o.Details().MinUnitsMatch)
	if err != nil {
		return nil, err
	}

	// Next, store any additional order data as a tlv stream.
	if err := storeOrderTlvTX(dst, nonce, o); err != nil {
		return nil, err
	}

	if bidOrder, ok := o.(*order.Bid); ok {
		err = storeOrderMinNoderTierTX(dst, nonce, bidOrder.MinNodeTier)
		if err != nil {
			return nil, err
		}
	}

	return o, nil
}

// copyOrder copies a single order from the source to destination bucket. No
// event references are copied, only the order itself.
func copyOrder(src, dst *bbolt.Bucket, nonce order.Nonce) error {
	var (
		o             order.Order
		orderBytes    []byte
		nodeTier      order.NodeTier
		minUnitsMatch order.SupplyUnit
		err           error
		callback      = func(_ order.Nonce, rawOrder []byte,
			extraData *extraOrderData) error {

			orderBytes = rawOrder

			// In order to know if we need the node tier at all,
			// we'll go ahead and decode fully this order in
			// question.
			r := bytes.NewReader(rawOrder)
			o, err = DeserializeOrder(nonce, r)
			if err != nil {
				return err
			}

			tlvReader := bytes.NewReader(extraData.tlvData)
			err = deserializeOrderTlvData(tlvReader, o)
			if err != nil {
				return err
			}

			nodeTier = extraData.minNodeTier
			minUnitsMatch = extraData.minUnitsMatch
			return nil
		}
	)

	// Retrieve the order stored in the source bucket.
	err = fetchOrderTX(src, nonce, callback)
	if err != nil {
		return err
	}

	// Store it in the new bucket.
	if err := storeOrderTX(dst, nonce, orderBytes, nil); err != nil {
		return err
	}

	// Next, store any additional order data as a tlv stream.
	if err := storeOrderTlvTX(dst, nonce, o); err != nil {
		return err
	}

	if _, ok := o.(*order.Bid); ok {
		err := storeOrderMinNoderTierTX(dst, nonce, nodeTier)
		if err != nil {
			return err
		}
	}
	return storeOrderMinUnitsMatchTX(dst, nonce, minUnitsMatch)
}

// SerializeOrder binary serializes an order to a writer using the common LN
// wire format.
func SerializeOrder(o order.Order, w *bytes.Buffer) error {
	kit := o.Details()

	// We don't have to deserialize the nonce as it's the sub bucket name.
	return codec.WriteElements(
		w, kit.Preimage, uint32(kit.Version), uint8(o.Type()),
		uint8(kit.State), kit.FixedRate, kit.Amt, uint64(kit.Units),
		kit.MultiSigKeyLocator, kit.MaxBatchFeeRate, kit.AcctKey,
		uint64(kit.UnitsUnfulfilled), kit.LeaseDuration,
	)
}

// DeserializeOrder deserializes an order from the binary LN wire format.
func DeserializeOrder(nonce order.Nonce, r io.Reader) (
	order.Order, error) {

	var (
		kit       = order.NewKit(nonce)
		orderType order.Type
	)

	// We don't serialize the nonce as it's part of the bucket name already.
	err := ReadElements(
		r, &kit.Preimage, &kit.Version, &orderType, &kit.State,
		&kit.FixedRate, &kit.Amt, &kit.Units, &kit.MultiSigKeyLocator,
		&kit.MaxBatchFeeRate, &kit.AcctKey, &kit.UnitsUnfulfilled,
		&kit.LeaseDuration,
	)
	if err != nil {
		return nil, err
	}

	// Now read the order type specific fields.
	switch orderType {
	case order.TypeAsk:
		return &order.Ask{
			Kit: *kit,
		}, nil

	case order.TypeBid:
		return &order.Bid{
			Kit: *kit,
		}, nil
	default:
		return nil, fmt.Errorf("unknown order type: %d", orderType)
	}
}

// deserializeOrderTlvData attempts to decode the remaining bytes in the
// supplied reader by interpreting it as a tlv stream. If successful any
// non-default values of the additional data will be set on the given order.
func deserializeOrderTlvData(r io.Reader, o order.Order) error {
	var (
		selfChanBalance            uint64
		sidecarTicket              []byte
		channelType                uint8
		allowedNodeIDs             []byte
		notAllowedNodeIDs          []byte
		bidUnannoucedChannel       uint8
		askAnnouncementConstraints uint8
		bidZeroConf                uint8
		askConfirmationConstraints uint8
		auctionType                uint8
		isPublic                   uint8
	)

	// We'll add records for all possible additional order data fields here
	// but will check below which of them were actually set, depending on
	// the order type as well.
	tlvStream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			bidSelfChanBalanceType, &selfChanBalance,
		),
		tlv.MakePrimitiveRecord(bidSidecarTicketType, &sidecarTicket),
		tlv.MakePrimitiveRecord(orderChannelType, &channelType),
		tlv.MakePrimitiveRecord(allowedNodeIDsType, &allowedNodeIDs),
		tlv.MakePrimitiveRecord(
			notAllowedNodeIDsType, &notAllowedNodeIDs,
		),
		tlv.MakePrimitiveRecord(
			bidUnannouncedChannelType, &bidUnannoucedChannel,
		),
		tlv.MakePrimitiveRecord(
			askChannelAnnouncementConstraintsType,
			&askAnnouncementConstraints,
		),
		tlv.MakePrimitiveRecord(bidZeroConfType, &bidZeroConf),
		tlv.MakePrimitiveRecord(
			askChannelConfirmationConstraintsType,
			&askConfirmationConstraints,
		),
		tlv.MakePrimitiveRecord(orderAuctionType, &auctionType),
		tlv.MakePrimitiveRecord(orderIsPublicType, &isPublic),
	)
	if err != nil {
		return err
	}

	parsedTypes, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	// Now check what records were actually parsed from the stream and
	// assign any parsed fields to our order.
	switch castOrder := o.(type) {
	case *order.Ask:
		t, ok := parsedTypes[askChannelAnnouncementConstraintsType]
		if ok && t == nil {
			constraint := order.ChannelAnnouncementConstraints(
				askAnnouncementConstraints,
			)
			castOrder.AnnouncementConstraints = constraint
		}

		t, ok = parsedTypes[askChannelConfirmationConstraintsType]
		if ok && t == nil {
			constraint := order.ChannelConfirmationConstraints(
				askConfirmationConstraints,
			)
			castOrder.ConfirmationConstraints = constraint
		}

	case *order.Bid:
		if t, ok := parsedTypes[bidSelfChanBalanceType]; ok && t == nil {
			castOrder.SelfChanBalance = btcutil.Amount(
				selfChanBalance,
			)
		}

		if t, ok := parsedTypes[bidSidecarTicketType]; ok && t == nil {
			castOrder.SidecarTicket, err = sidecar.DeserializeTicket(
				bytes.NewReader(sidecarTicket),
			)
			if err != nil {
				return err
			}
		}

		t, ok := parsedTypes[bidUnannouncedChannelType]
		if ok && t == nil && bidUnannoucedChannel == 1 {
			castOrder.UnannouncedChannel = true
		}

		t, ok = parsedTypes[bidZeroConfType]
		if ok && t == nil && bidZeroConf == 1 {
			castOrder.ZeroConfChannel = true
		}
	}

	if t, ok := parsedTypes[orderChannelType]; ok && t == nil {
		o.Details().ChannelType = order.ChannelType(channelType)
	}

	if t, ok := parsedTypes[allowedNodeIDsType]; ok && t == nil {
		nodeIDs, err := AssemblePubKeySlice(allowedNodeIDs)
		if err != nil {
			return fmt.Errorf("invalid allowedNodeIDs: %v", err)
		}
		o.Details().AllowedNodeIDs = nodeIDs
	}

	if t, ok := parsedTypes[notAllowedNodeIDsType]; ok && t == nil {
		nodeIDs, err := AssemblePubKeySlice(notAllowedNodeIDs)
		if err != nil {
			return fmt.Errorf("invalid notAllowedNodeIDs: %v", err)
		}
		o.Details().NotAllowedNodeIDs = nodeIDs
	}

	if t, ok := parsedTypes[orderAuctionType]; ok && t == nil {
		o.Details().AuctionType = order.AuctionType(auctionType)
	}

	t, ok := parsedTypes[orderIsPublicType]
	if ok && t == nil && isPublic == 1 {
		o.Details().IsPublic = true
	}

	return nil
}

// serializeOrderTlvData encodes all additional data of an order as a single tlv
// stream.
func serializeOrderTlvData(w io.Writer, o order.Order) error {
	var (
		tlvRecords                 []tlv.Record
		askAnnouncementConstraints uint8
		bidUnannouncedChannel      uint8
		askConfirmationConstraints uint8
		bidZeroConfChannel         uint8
	)

	switch castOrder := o.(type) {
	case *order.Ask:
		askAnnouncementConstraints = uint8(
			castOrder.AnnouncementConstraints,
		)

		askConfirmationConstraints = uint8(
			castOrder.ConfirmationConstraints,
		)

	case *order.Bid:
		if castOrder.SelfChanBalance != 0 {
			selfChanBalance := uint64(castOrder.SelfChanBalance)
			tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
				bidSelfChanBalanceType, &selfChanBalance,
			))
		}

		if castOrder.SidecarTicket != nil {
			var buf bytes.Buffer
			if err := sidecar.SerializeTicket(
				&buf, castOrder.SidecarTicket,
			); err != nil {
				return err
			}

			sidecarBytes := buf.Bytes()
			tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
				bidSidecarTicketType, &sidecarBytes,
			))
		}

		if castOrder.UnannouncedChannel {
			bidUnannouncedChannel = uint8(1)
		}

		if castOrder.ZeroConfChannel {
			bidZeroConfChannel = uint8(1)
		}
	}

	channelType := uint8(o.Details().ChannelType)
	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		orderChannelType, &channelType,
	))

	if len(o.Details().AllowedNodeIDs) > 0 {
		idBytes := FlattenPubKeySlice(o.Details().AllowedNodeIDs)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				allowedNodeIDsType, &idBytes,
			),
		)
	}

	if len(o.Details().NotAllowedNodeIDs) > 0 {
		idBytes := FlattenPubKeySlice(o.Details().NotAllowedNodeIDs)
		tlvRecords = append(
			tlvRecords, tlv.MakePrimitiveRecord(
				notAllowedNodeIDsType, &idBytes,
			),
		)
	}

	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		bidUnannouncedChannelType, &bidUnannouncedChannel,
	))

	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		askChannelAnnouncementConstraintsType,
		&askAnnouncementConstraints,
	))

	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		bidZeroConfType, &bidZeroConfChannel,
	))

	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		askChannelConfirmationConstraintsType,
		&askConfirmationConstraints,
	))

	auctionType := uint8(o.Details().AuctionType)
	tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
		orderAuctionType, &auctionType,
	))

	if o.Details().IsPublic {
		isPublic := uint8(1)
		tlvRecords = append(tlvRecords, tlv.MakePrimitiveRecord(
			orderIsPublicType, &isPublic,
		))
	}

	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// FlattenPubKeySlice returns a flatten array of bytes from the
// given array of public keys.
func FlattenPubKeySlice(pubKeys [][33]byte) []byte {
	flatten := make([]byte, len(pubKeys)*33)

	for i, key := range pubKeys {
		copy(flatten[i*33:(i+1)*33], key[:])
	}

	return flatten
}

// AssemblePubKeySlice is the inverse of FlattenPubKeySlice. Given a slice of
// bytes representing a flattened slice of pubkeys returns the original slice
// of pubkeys.
func AssemblePubKeySlice(flatten []byte) ([][33]byte, error) {
	// Sanity check that this is a valid encoded slice.
	if len(flatten)%33 != 0 {
		return nil, fmt.Errorf("invalid length for a [][33]byte "+
			"encoded slice: %v", len(flatten))
	}
	numOfPubKeys := len(flatten) / 33
	pubKeys := make([][33]byte, 0, numOfPubKeys)

	for i := 0; i < numOfPubKeys; i++ {
		var pubKey [33]byte
		copy(pubKey[:], flatten[i*33:(i+1)*33])
		pubKeys = append(pubKeys, pubKey)
	}
	return pubKeys, nil
}
