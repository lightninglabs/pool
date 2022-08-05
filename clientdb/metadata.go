package clientdb

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/pool/clientdb/migrations"
	"go.etcd.io/bbolt"
)

// migration is a function which takes a prior outdated version of the database
// instance and mutates the key/bucket structure to arrive at a more up-to-date
// version of the database.
type migration func(tx *bbolt.Tx) error

var (
	// metadataBucketKey stores all the metadata concerning the state of the
	// database.
	metadataBucketKey = []byte("metadata")

	// dbVersionKey is the key used for storing/retrieving the current
	// database version.
	dbVersionKey = []byte("version")

	// lockIDKey is the database key used for storing/retrieving the global
	// lock ID to use when leasing outputs from the backing lnd node's
	// wallet. This is mostly required so that calls to LeaseOutput are
	// idempotent when attempting to lease an output we already have a lease
	// for.
	lockIDKey = []byte("lock-id")

	// ErrDBReversion is returned when detecting an attempt to revert to a
	// prior database version.
	ErrDBReversion = errors.New("cannot revert to prior version")

	// dbVersions is storing all versions of database. If current version
	// of database don't match with latest version this list will be used
	// for retrieving all migration function that are need to apply to the
	// current db.
	dbVersions = []migration{
		migrations.AddInitialOrderTimestamps,
	}

	latestDBVersion = uint32(len(dbVersions))
)

// getDBVersion retrieves the current database version.
func getDBVersion(bucket *bbolt.Bucket) (uint32, error) {
	versionBytes := bucket.Get(dbVersionKey)
	if versionBytes == nil {
		return 0, errors.New("database version not found")
	}
	return byteOrder.Uint32(versionBytes), nil
}

// setDBVersion updates the current database version.
func setDBVersion(bucket *bbolt.Bucket, version uint32) error {
	var b [4]byte
	byteOrder.PutUint32(b[:], version)
	return bucket.Put(dbVersionKey, b[:])
}

// getBucket retrieves the bucket with the given key.
func getBucket(tx *bbolt.Tx, key []byte) (*bbolt.Bucket, error) {
	bucket := tx.Bucket(key)
	if bucket == nil {
		return nil, fmt.Errorf("bucket \"%v\" does not exist",
			string(key))
	}
	return bucket, nil
}

// getNestedBucket retrieves the nested bucket with the given key found within
// the given bucket. If the bucket does not exist and `create` is true, then the
// bucket is created.
func getNestedBucket(bucket *bbolt.Bucket, key []byte,
	create bool) (*bbolt.Bucket, error) {

	nestedBucket := bucket.Bucket(key)
	if nestedBucket == nil && create {
		return bucket.CreateBucketIfNotExists(key)
	}
	if nestedBucket == nil {
		return nil, fmt.Errorf("nested bucket \"%v\" does not exist",
			string(key))
	}
	return nestedBucket, nil
}

// syncVersions function is used for safe db version synchronization. It
// applies migration functions to the current database and recovers the
// previous state of db if at least one error/panic appeared during migration.
func syncVersions(db *bbolt.DB) error {
	var currentVersion uint32
	err := db.View(func(tx *bbolt.Tx) error {
		metadata, err := getBucket(tx, metadataBucketKey)
		if err != nil {
			return err
		}
		currentVersion, err = getDBVersion(metadata)
		return err
	})
	if err != nil {
		return err
	}

	log.Infof("Checking for schema update: latest_version=%v, "+
		"db_version=%v", latestDBVersion, currentVersion)

	switch {
	// If the database reports a higher version that we are aware of, the
	// user is probably trying to revert to a prior version of lnd. We fail
	// here to prevent reversions and unintended corruption.
	case currentVersion > latestDBVersion:
		log.Errorf("Refusing to revert from db_version=%d to "+
			"lower version=%d", currentVersion,
			latestDBVersion)

		return ErrDBReversion

	// If the current database version matches the latest version number,
	// then we don't need to perform any migrations.
	case currentVersion == latestDBVersion:
		return nil
	}

	log.Infof("Performing database schema migration")

	// Otherwise we execute the migrations serially within a single database
	// transaction to ensure the migration is atomic.
	return db.Update(func(tx *bbolt.Tx) error {
		for v := currentVersion; v < latestDBVersion; v++ {
			log.Infof("Applying migration #%v", v+1)

			migration := dbVersions[v]
			if err := migration(tx); err != nil {
				log.Infof("Unable to apply migration #%v", v+1)
				return err
			}
		}

		metadata, err := getBucket(tx, metadataBucketKey)
		if err != nil {
			return err
		}
		return setDBVersion(metadata, latestDBVersion)
	})
}

// storeRandomLockID generates a random lock ID backed by the system's CSPRNG
// and stores it under the metadata bucket.
func storeRandomLockID(metadata *bbolt.Bucket) error {
	var lockID wtxmgr.LockID
	if _, err := rand.Read(lockID[:]); err != nil {
		return err
	}
	return metadata.Put(lockIDKey, lockID[:])
}

// LockID retrieves the database's global lock ID used to lease outputs from the
// backing lnd node's wallet.
func (db *DB) LockID() (wtxmgr.LockID, error) {
	var lockID wtxmgr.LockID
	err := db.View(func(tx *bbolt.Tx) error {
		metadata, err := getBucket(tx, metadataBucketKey)
		if err != nil {
			return err
		}

		lockIDBytes := metadata.Get(lockIDKey)
		if lockIDBytes == nil {
			return errors.New("lock ID not found")
		}

		copy(lockID[:], lockIDBytes)
		return nil
	})
	return lockID, err
}
