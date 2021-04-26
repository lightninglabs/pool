package clientdb

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	// DBFilename is the default filename of the client database.
	DBFilename = "pool.db"

	// DefaultPoolDBTimeout is the default maximum time we wait for the
	// Pool bbolt database to be opened. If the database is already opened
	// by another process, the unique lock cannot be obtained. With the
	// timeout we error out after the given time instead of just blocking
	// for forever.
	DefaultPoolDBTimeout = 5 * time.Second
)

var (
	// byteOrder is the default byte order we'll use for serialization
	// within the database.
	byteOrder = binary.BigEndian
)

// DB is a bolt-backed persistent store.
type DB struct {
	walletdb.DB
}

// New creates a new bolt database that can be found at the given directory.
func New(dir, fileName string) (*DB, error) {
	path := filepath.Join(dir, fileName)

	// Detect first init of database for JS.
	dbExists, err := isExistingDB(dir, path)
	if err != nil {
		return nil, err
	}

	log.Debugf("Database %s exists: %v", path, dbExists)

	db, err := initDB(path, !dbExists)
	if err != nil {
		return nil, err
	}

	// Attempt to sync the database's current version with the latest known
	// version available.
	if err := syncVersions(db); err != nil {
		return nil, err
	}

	return &DB{DB: db}, nil
}

// fileExists reports whether the named file or directory exists.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// initDB initializes all of the required top-level buckets for the database.
func initDB(filepath string, firstInit bool) (walletdb.DB, error) {
	var (
		db  walletdb.DB
		err error
	)
	if !lnrpc.FileExists(filepath) {
		db, err = walletdb.Create(dbDriver, filepath, false, time.Second)
		if err != nil {
			return nil, err
		}
	} else {
		db, err = walletdb.Open(dbDriver, filepath, false, time.Second)
		if err != nil {
			return nil, err
		}
	}

	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		if firstInit {
			metadataBucket, err := tx.CreateTopLevelBucket(
				metadataBucketKey,
			)
			if err != nil {
				return err
			}
			err = setDBVersion(metadataBucket, latestDBVersion)
			if err != nil {
				return err
			}
			if err := storeRandomLockID(metadataBucket); err != nil {
				return err
			}
		}

		_, err = tx.CreateTopLevelBucket(accountBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateTopLevelBucket(ordersBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateTopLevelBucket(sidecarsBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateTopLevelBucket(batchBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateTopLevelBucket(eventBucketKey)
		if err != nil {
			return err
		}
		snapshotBucket, err := tx.CreateTopLevelBucket(
			batchSnapshotBucketKey,
		)
		if err != nil {
			return err
		}

		_, err = snapshotBucket.CreateBucketIfNotExists(
			batchSnapshotSeqBucketKey,
		)
		if err != nil {
			return err
		}

		_, err = snapshotBucket.CreateBucketIfNotExists(
			batchSnapshotBatchIDIndexBucketKey,
		)
		return err
	})
	if err != nil {
		return nil, err
	}

	return db, nil
}
