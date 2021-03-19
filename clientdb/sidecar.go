package clientdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
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

// Sidecars retrieves all known sidecars from the database.
func (db *DB) Sidecars() ([]*sidecar.Ticket, error) {
	var res []*sidecar.Ticket
	err := db.View(func(tx *bbolt.Tx) error {
		sidecarBucket, err := getBucket(tx, sidecarsBucketKey)
		if err != nil {
			return err
		}

		return sidecarBucket.ForEach(func(k, v []byte) error {
			// We don't expect any sub-buckets with sidecars.
			if v == nil {
				return fmt.Errorf("nil value for key %x", k)
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
