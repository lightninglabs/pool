package clientdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/pool/order"
	"github.com/lightninglabs/pool/sidecar"
	"go.etcd.io/bbolt"
)

var (
	// ErrNoSidecar is the error returned if no sidecar with the given
	// multisig pubkey exists in the store.
	ErrNoSidecar = errors.New("no sidecar found")

	// sidecarsBucketKey is a bucket that contains all sidecars that are
	// currently pending or completed. This bucket is keyed by the ticket ID
	// and offer signing pubkey of a sidecar.
	sidecarsBucketKey = []byte("sidecars")

	// bidTemplateBucket is a bucket that's used to store the order
	// template of a sidecar ticket for the provider to be able to execute
	// automated negotiation of the order.
	bidTemplateBucket = []byte("sidecar-bids")
)

const (
	// sidecarKeyLen is the length of a sidecar ticket's key. It is the
	// length of the sidecar ID (8 bytes) plus the length of a compressed
	// public key (33 bytes).
	sidecarKeyLen = 8 + 33
)

// A compile time check to make sure we satisfy the sidecar.Store interface.
var _ sidecar.Store = (*DB)(nil)

// getSidecarKey returns the key for a sidecar.
func getSidecarKey(id [8]byte, offerSignPubKey *btcec.PublicKey) ([]byte,
	error) {

	if offerSignPubKey == nil {
		return nil, fmt.Errorf("offer signing pubkey cannot be nil")
	}

	var result [sidecarKeyLen]byte

	copy(result[:], id[:])
	copy(result[8:], offerSignPubKey.SerializeCompressed())

	return result[:], nil
}

// AddSidecar adds a record for the sidecar to the database.
func (db *DB) AddSidecar(ticket *sidecar.Ticket) error {
	sidecarKey, err := getSidecarKey(ticket.ID, ticket.Offer.SignPubKey)
	if err != nil {
		return err
	}

	return db.Update(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		sidecarValue := sidecarBucket.Get(sidecarKey)
		if len(sidecarValue) != 0 {
			return fmt.Errorf("sidecar for key %x already exists",
				sidecarKey)
		}

		return storeSidecar(sidecarBucket, sidecarKey, ticket)
	})
}

// AddSidecarWithBid is identical to the AddSidecar method, but it also inserts
// a bid template in a special bucket to facilitate automated negotiation of
// sidecar channels.
func (db *DB) AddSidecarWithBid(ticket *sidecar.Ticket, bid *order.Bid) error {
	sidecarKey, err := getSidecarKey(ticket.ID, ticket.Offer.SignPubKey)
	if err != nil {
		return err
	}

	// Although the order hasn't fully advanced set to the state where we
	// sign+commit to the order nonce itself, since we already know it at
	// this point, we can just apply it directly to the ticket.
	ticket.Order = new(sidecar.Order)
	bidNonce := bid.Nonce()
	copy(ticket.Order.BidNonce[:], bidNonce[:])

	return db.Update(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}
		sidecarValue := sidecarBucket.Get(sidecarKey)
		if len(sidecarValue) != 0 {
			return fmt.Errorf("sidecar for key %x already exists",
				sidecarKey)
		}

		err = storeSidecar(sidecarBucket, sidecarKey, ticket)
		if err != nil {
			return err
		}

		bidBucket, err := sidecarBucket.CreateBucketIfNotExists(
			bidTemplateBucket,
		)
		if err != nil {
			return err
		}

		return storeBidTemplate(bidBucket, bid, bidNonce)
	})
}

// UpdateSidecar updates a sidecar in the database.
func (db *DB) UpdateSidecar(ticket *sidecar.Ticket) error {
	sidecarKey, err := getSidecarKey(ticket.ID, ticket.Offer.SignPubKey)
	if err != nil {
		return err
	}

	return db.Update(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		sidecarValue := sidecarBucket.Get(sidecarKey)
		if len(sidecarValue) == 0 {
			return ErrNoSidecar
		}

		// If the ticket is in a terminal state, we won't ever need the
		// bid template again, so we remove it (if it still exists).
		if ticket.State.IsTerminal() && ticket.Order != nil {
			err := removeBidTemplate(
				sidecarBucket, ticket.Order.BidNonce,
			)
			if err != nil {
				return err
			}
		}

		return storeSidecar(sidecarBucket, sidecarKey, ticket)
	})
}

// Sidecar retrieves a specific sidecar by its ID and provider signing key
// (offer signature pubkey) or returns ErrNoSidecar if it's not found.
func (db *DB) Sidecar(id [8]byte,
	offerSignPubKey *btcec.PublicKey) (*sidecar.Ticket, error) {

	sidecarKey, err := getSidecarKey(id, offerSignPubKey)
	if err != nil {
		return nil, err
	}

	var s *sidecar.Ticket
	err = db.View(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		s, err = readSidecar(sidecarBucket, sidecarKey)
		return err
	})
	if err != nil {
		return nil, err
	}

	return s, nil
}

// SidecarsByID returns all sidecar tickets with the given ID. Normally this is
// just a single ticket. But because the ID is just 8 bytes and is randomly
// generated, there could be collisions, especially since tickets can also be
// crafted by a malicious party and given to any node. That's why the offer's
// public key is also used as an identifying element since that cannot easily be
// forged without also producing a valid signature. So an attacker cannot
// overwrite a ticket a node offered by themselves offering a ticket with the
// same ID and tricking the victim into registering that.
func (db *DB) SidecarsByID(id [8]byte) ([]*sidecar.Ticket, error) {
	var res []*sidecar.Ticket
	err := db.View(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		// The first 8 bytes of the key is a sidecar ticket's ID. So as
		// long as the key's prefix is matching, we still have tickets
		// with the same ID.
		cursor := sidecarBucket.Cursor()
		key, val := cursor.Seek(id[:])
		for ; key != nil && bytes.HasPrefix(key, id[:]); key, val = cursor.Next() {
			// The main sidecar bucket has a sub-bucket that's used
			// to store order bid information, so we'll skip this
			// bucket when attempting to read out all the tickets.
			if val == nil {
				return nil
			}

			s, err := readSidecar(sidecarBucket, key)
			if err != nil {
				return err
			}
			res = append(res, s)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

// SidecarBidTemplate attempts to retrieve a bid template associated with the
// passed sidecar ticket.
func (db *DB) SidecarBidTemplate(ticket *sidecar.Ticket) (*order.Bid, error) {
	var bid *order.Bid

	err := db.View(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		bidBucket := sidecarBucket.Bucket(bidTemplateBucket)
		if bidBucket == nil {
			return err
		}

		bid, err = readBidTemplate(bidBucket, ticket.Order.BidNonce)
		return err
	})
	if err != nil {
		return nil, err
	}

	return bid, nil
}

// Sidecars retrieves all known sidecars from the database.
func (db *DB) Sidecars() ([]*sidecar.Ticket, error) {
	var res []*sidecar.Ticket
	err := db.View(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		return sidecarBucket.ForEach(func(k, v []byte) error {
			// The main sidecar bucket has a sub-bucket that's used
			// to store order bid information, so we'll skip this
			// bucket when attempting to read out all the tickets.
			if v == nil {
				return nil
			}

			s, err := readSidecar(sidecarBucket, k)
			if err != nil {
				return err
			}
			res = append(res, s)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return res, nil
}

func storeSidecar(targetBucket *bbolt.Bucket, key []byte,
	ticket *sidecar.Ticket) error {

	var sidecarBuf bytes.Buffer
	if err := sidecar.SerializeTicket(&sidecarBuf, ticket); err != nil {
		return err
	}

	// TODO(roasbeef): store bid along side in new key?

	return targetBucket.Put(key, sidecarBuf.Bytes())
}

func readSidecar(sourceBucket *bbolt.Bucket, id []byte) (*sidecar.Ticket,
	error) {

	sidecarBytes := sourceBucket.Get(id)
	if sidecarBytes == nil {
		return nil, ErrNoSidecar
	}

	return sidecar.DeserializeTicket(bytes.NewReader(sidecarBytes))
}

func storeBidTemplate(bidBucket *bbolt.Bucket, bid *order.Bid,
	ticketNonce order.Nonce) error {

	var w bytes.Buffer
	if err := SerializeOrder(bid, &w); err != nil {
		return err
	}

	err := storeOrderTX(bidBucket, ticketNonce, w.Bytes(), nil)
	if err != nil {
		return err
	}
	err = storeOrderMinUnitsMatchTX(
		bidBucket, ticketNonce, bid.Details().MinUnitsMatch,
	)
	if err != nil {
		return err
	}
	if err := storeOrderTlvTX(bidBucket, ticketNonce, bid); err != nil {
		return err
	}
	return storeOrderMinNoderTierTX(bidBucket, ticketNonce, bid.MinNodeTier)
}

func readBidTemplate(bidBucket *bbolt.Bucket,
	ticketNonce order.Nonce) (*order.Bid, error) {

	var (
		o   order.Order
		err error
	)

	callback := func(nonce order.Nonce, rawOrder []byte,
		extraData *extraOrderData) error {

		r := bytes.NewReader(rawOrder)
		o, err = DeserializeOrder(nonce, r)
		if err != nil {
			return err
		}

		tlvReader := bytes.NewReader(extraData.tlvData)
		err := deserializeOrderTlvData(tlvReader, o)
		if err != nil {
			return err
		}

		if bidOrder, ok := o.(*order.Bid); ok {
			bidOrder.MinNodeTier = extraData.minNodeTier
		}
		o.Details().MinUnitsMatch = extraData.minUnitsMatch

		return nil
	}

	err = fetchOrderTX(bidBucket, ticketNonce, callback)
	if err != nil {
		return nil, err
	}

	return o.(*order.Bid), nil
}

func removeBidTemplate(sidecarBucket *bbolt.Bucket,
	ticketNonce order.Nonce) error {

	bidBucket := sidecarBucket.Bucket(bidTemplateBucket)

	// If there is no bucket for the bid templates yet, we don't have
	// anything to do.
	if bidBucket == nil {
		return nil
	}

	// If the ticket nonce wasn't set, there won't be a template stored for
	// it either.
	if ticketNonce == order.ZeroNonce {
		return nil
	}

	// If the template was already removed, we ignore the error.
	err := bidBucket.DeleteBucket(ticketNonce[:])
	if err != bbolt.ErrBucketNotFound {
		return err
	}

	return nil
}
