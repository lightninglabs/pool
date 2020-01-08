package clientdb

import (
	"encoding/binary"
	"os"
	"path/filepath"

	"github.com/coreos/bbolt"
)

const (
	// dbFilename is the default filename of the client database.
	dbFilename = "agora.db"

	// dbFilePermission is the default permission the client database file
	// is created with.
	dbFilePermission = 0600
)

var (
	// byteOrder is the default byte order we'll use for serialization
	// within the database.
	byteOrder = binary.BigEndian
)

// DB is a bolt-backed persistent store.
type DB struct {
	*bbolt.DB
}

// New creates a new bolt database that can be found at the given directory.
func New(dir string) (*DB, error) {
	firstInit := false
	path := filepath.Join(dir, dbFilename)

	// If the database file does not exist yet, create its directory.
	if !fileExists(path) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		firstInit = true
	}

	db, err := initDB(path, firstInit)
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
func initDB(filepath string, firstInit bool) (*bbolt.DB, error) {
	db, err := bbolt.Open(filepath, dbFilePermission, nil)
	if err != nil {
		return nil, err
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		if firstInit {
			metadataBucket, err := tx.CreateBucketIfNotExists(
				metadataBucketKey,
			)
			if err != nil {
				return err
			}
			err = setDBVersion(metadataBucket, latestDBVersion)
			if err != nil {
				return err
			}
		}

		_, err = tx.CreateBucketIfNotExists(accountBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(ordersBucketKey)
		return err
	})
	if err != nil {
		return nil, err
	}

	return db, nil
}
