package clientdb

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall/js"

	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	dbDriver = "localStorage"
)

var (
	errNotSupported = fmt.Errorf("not supported")
)

func init() {
	// Register the driver.
	driver := walletdb.Driver{
		DbType: dbDriver,
		Create: openDB,
		Open:   openDB,
	}
	if err := walletdb.RegisterDriver(driver); err != nil {
		panic(fmt.Sprintf("Failed to regiser database driver '%s': %v",
			dbDriver, err))
	}
}

func isExistingDB(_, _ string) (bool, error) {
	ok := DetectStorage()
	if !ok {
		return false, ErrLocalStorageNotSupported
	}

	_, err := getDBVersion(&transaction{path: string(metadataBucketKey)})
	if err == ErrDBVersionNotFound {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// openDB opens the database at the provided path.  walletdb.ErrDbDoesNotExist
// is returned if the database doesn't exist and the create flag is not set.
func openDB(...interface{}) (walletdb.DB, error) {
	ok := DetectStorage()
	if !ok {
		return nil, ErrLocalStorageNotSupported
	}

	return &db{}, nil
}

type transaction struct {
	path string
}

func (tx *transaction) subKey(key []byte) string {
	return strings.Join([]string{tx.path, string(key)}, "/")
}

func (tx *transaction) ReadBucket(key []byte) walletdb.ReadBucket {
	log.Debugf("ReadBucket(%s)", string(key))
	return tx.ReadWriteBucket(key)
}

func (tx *transaction) ReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	log.Debugf("ReadWriteBucket(%s)", string(key))
	return &transaction{path: string(key)}
}

func (tx *transaction) CreateTopLevelBucket(
	key []byte) (walletdb.ReadWriteBucket, error) {

	return tx.ReadWriteBucket(key), nil
}

func (tx *transaction) DeleteTopLevelBucket([]byte) error {
	return fmt.Errorf("not supported")
}

// Commit commits all changes that have been made through the root bucket and
// all of its sub-buckets to persistent storage.
//
// This function is part of the walletdb.ReadWriteTx interface implementation.
func (tx *transaction) Commit() error {
	return nil
}

// Rollback undoes all changes that have been made to the root bucket and all of
// its sub-buckets.
//
// This function is part of the walletdb.ReadTx interface implementation.
func (tx *transaction) Rollback() error {
	return nil
}

// OnCommit takes a function closure that will be executed when the transaction
// successfully gets committed.
//
// This function is part of the walletdb.ReadWriteTx interface implementation.
func (tx *transaction) OnCommit(f func()) {
	f()
}

// NestedReadWriteBucket retrieves a nested bucket with the given key.  Returns
// nil if the bucket does not exist.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) NestedReadWriteBucket(key []byte) walletdb.ReadWriteBucket {
	log.Debugf("NestedReadWriteBucket(%s)", string(key))
	return &transaction{path: tx.subKey(key)}
}

func (tx *transaction) NestedReadBucket(key []byte) walletdb.ReadBucket {
	log.Debugf("NestedReadBucket(%s)", string(key))
	return tx.NestedReadWriteBucket(key)
}

// CreateBucket creates and returns a new nested bucket with the given key.
// Returns ErrBucketExists if the bucket already exists, ErrBucketNameRequired
// if the key is empty, or ErrIncompatibleValue if the key value is otherwise
// invalid.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) CreateBucket(key []byte) (walletdb.ReadWriteBucket, error) {
	log.Debugf("CreateBucket(%s)", string(key))
	return tx.NestedReadWriteBucket(key), nil
}

// CreateBucketIfNotExists creates and returns a new nested bucket with the
// given key if it does not already exist.  Returns ErrBucketNameRequired if the
// key is empty or ErrIncompatibleValue if the key value is otherwise invalid.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) CreateBucketIfNotExists(
	key []byte) (walletdb.ReadWriteBucket, error) {

	log.Debugf("CreateBucketIfNotExists(%s)", string(key))
	return tx.NestedReadWriteBucket(key), nil
}

// DeleteNestedBucket removes a nested bucket with the given key.  Returns
// ErrTxNotWritable if attempted against a read-only transaction and
// ErrBucketNotFound if the specified bucket does not exist.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) DeleteNestedBucket([]byte) error {
	return errNotSupported
}

// ForEach invokes the passed function with every key/value pair in the bucket.
// This includes nested buckets, in which case the value is nil, but it does not
// include the key/value pairs within those nested buckets.
//
// NOTE: The values returned by this function are only valid during a
// transaction.  Attempting to access them after a transaction has ended will
// likely result in an access violation.
//
// This function is part of the walletdb.ReadBucket interface implementation.
func (tx *transaction) ForEach(fn func(k, v []byte) error) error {
	return ForEach(tx.path, func(key, valueBase64 string) error {
		value, err := base64.StdEncoding.DecodeString(valueBase64)
		if err != nil {
			return err
		}

		return fn([]byte(key), value)
	})
}

func (tx *transaction) ForEachBucket(fn func(k []byte) error) error {
	return errNotSupported
}

// Put saves the specified key/value pair to the bucket.  Keys that do not
// already exist are added and keys that already exist are overwritten.  Returns
// ErrTxNotWritable if attempted against a read-only transaction.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) Put(key, value []byte) error {
	fullKey := tx.subKey(key)
	log.Debugf("Put(%s, %s)", fullKey, string(value))
	return SetItem(fullKey, base64.StdEncoding.EncodeToString(value))
}

// Get returns the value for the given key.  Returns nil if the key does
// not exist in this bucket (or nested buckets).
//
// NOTE: The value returned by this function is only valid during a
// transaction.  Attempting to access it after a transaction has ended
// will likely result in an access violation.
//
// This function is part of the walletdb.ReadBucket interface implementation.
func (tx *transaction) Get(key []byte) []byte {
	fullKey := tx.subKey(key)
	log.Debugf("Get(%s)", fullKey)
	valueBase64, err := GetItem(fullKey)
	if err != nil {
		return nil
	}

	value, err := base64.StdEncoding.DecodeString(valueBase64)
	if err != nil {
		return nil
	}
	return value
}

// Delete removes the specified key from the bucket.  Deleting a key that does
// not exist does not return an error.  Returns ErrTxNotWritable if attempted
// against a read-only transaction.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) Delete(key []byte) error {
	fullKey := tx.subKey(key)
	log.Debugf("Delete(%s)", fullKey)
	return RemoveItem(fullKey)
}

func (tx *transaction) ReadCursor() walletdb.ReadCursor {
	return nil
}

// ReadWriteCursor returns a new cursor, allowing for iteration over the bucket's
// key/value pairs and nested buckets in forward or backward order.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) ReadWriteCursor() walletdb.ReadWriteCursor {
	return nil
}

// Tx returns the bucket's transaction.
//
// This function is part of the walletdb.ReadWriteBucket interface implementation.
func (tx *transaction) Tx() walletdb.ReadWriteTx {
	return tx
}

// NextSequence returns an autoincrementing integer for the bucket.
func (tx *transaction) NextSequence() (uint64, error) {
	return 0, errNotSupported
}

// SetSequence updates the sequence number for the bucket.
func (tx *transaction) SetSequence(uint64) error {
	return errNotSupported
}

// Sequence returns the current integer for the bucket without incrementing it.
func (tx *transaction) Sequence() uint64 {
	return 0
}

// db represents a collection of namespaces which are persisted and implements
// the walletdb.Db interface.  All database access is performed through
// transactions which are obtained through the specific Namespace.
type db struct {
}

// Enforce db implements the walletdb.Db interface.
var _ walletdb.DB = (*db)(nil)

func (db *db) beginTx() (*transaction, error) {
	return &transaction{}, nil
}

func (db *db) BeginReadTx() (walletdb.ReadTx, error) {
	return db.beginTx()
}

func (db *db) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	return db.beginTx()
}

// Copy writes a copy of the database to the provided writer.  This call will
// start a read-only transaction to perform all operations.
//
// This function is part of the walletdb.Db interface implementation.
func (db *db) Copy(io.Writer) error {
	return errNotSupported
}

// Close cleanly shuts down the database and syncs all data.
//
// This function is part of the walletdb.Db interface implementation.
func (db *db) Close() error {
	return nil
}

// Batch is similar to the package-level Update method, but it will attempt to
// optismitcally combine the invocation of several transaction functions into a
// single db write transaction.
//
// This function is part of the walletdb.Db interface implementation.
func (db *db) Batch(f func(tx walletdb.ReadWriteTx) error) error {
	tx, err := db.BeginReadWriteTx()
	if err != nil {
		return err
	}

	return f(tx)
}

var localStorage js.Value

// ErrLocalStorageNotSupported is returned if localStorage is not supported.
var ErrLocalStorageNotSupported = errors.New("localStorage does not appear " +
	"to be supported/enabled in this browser")

// ItemNotFoundError is returned if an item with the given key does not exist in
// localStorage.
type ItemNotFoundError struct {
	msg string
}

// Error implements the error interface.
func (e ItemNotFoundError) Error() string {
	return e.msg
}

func newItemNotFoundError(format string, args ...interface{}) ItemNotFoundError {
	return ItemNotFoundError{
		msg: fmt.Sprintf(format, args...),
	}
}

// DetectStorage detects and (re)initializes the localStorage.
func DetectStorage() (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Debugf("Recovered from panic in DetectStorage: %v",
				r)
			localStorage = js.Undefined()
			ok = false
		}
	}()

	localStorage = js.Global().Get("localStorage")

	if localStorage.IsUndefined() {
		log.Debugf("Value is undefined")
		return false
	}

	// Cf. https://developer.mozilla.org/en-US/docs/Web/API/Web_Storage_API/Using_the_Web_Storage_API
	// https://gist.github.com/paulirish/5558557
	x := "__storage_test__"
	localStorage.Set(x, x)
	obj := localStorage.Get(x)
	if obj.IsUndefined() {
		log.Debugf("Storage test return value nil")
		localStorage = js.Undefined()
		return false
	}
	localStorage.Call("removeItem", x)
	return true
}

// SetItem saves the given item in localStorage under the given key.
func SetItem(key, item string) (err error) {
	if localStorage.IsUndefined() {
		return ErrLocalStorageNotSupported
	}
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("could not use local "+
					"storage: %v", r)
			}
		}
	}()

	localStorage.Call("setItem", key, item)
	return nil
}

// GetItem finds and returns the item identified by key. If there is no item in
// localStorage with the given key, GetItem will return an ItemNotFoundError.
func GetItem(key string) (s string, err error) {
	if localStorage.IsUndefined() {
		return "", ErrLocalStorageNotSupported
	}
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("could not use local "+
					"storage: %v", r)
			}
			s = ""
		}
	}()

	item := localStorage.Call("getItem", key)
	if item.IsUndefined() {
		err = newItemNotFoundError(
			"Could not find an item with the given key: %s", key)
	} else {
		s = item.String()
	}
	return s, err
}

// Key finds and returns the key associated with the given item. If the item is
// not in localStorage, Key will return an ItemNotFoundError.
func Key(item string) (s string, err error) {
	if localStorage.IsUndefined() {
		return "", ErrLocalStorageNotSupported
	}
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("could not use local "+
					"storage: %v", r)
			}
			s = ""
		}
	}()

	key := localStorage.Call("key", item)
	if key.IsUndefined() {
		err = newItemNotFoundError(
			"Could not find a key for the given item: %s", item)
	} else {
		s = key.String()
	}
	return s, err
}

// RemoveItem removes the item with the given key from localStorage.
func RemoveItem(key string) (err error) {
	if localStorage.IsUndefined() {
		return ErrLocalStorageNotSupported
	}
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("could not use local "+
					"storage: %v", r)
			}
		}
	}()

	localStorage.Call("removeItem", key)
	return nil
}

// Length returns the number of items currently in localStorage.
func Length() (l int, err error) {
	if localStorage.IsUndefined() {
		return 0, ErrLocalStorageNotSupported
	}
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("could not use local "+
					"storage: %v", r)
			}
			l = 0
		}
	}()

	length := localStorage.Get("length")
	return length.Int(), nil
}

func ForEach(prefix string, f func(key, value string) error) error {
	length, err := Length()
	if err != nil {
		return err
	}
	for i := 0; i < length; i++ {
		strKey, err := Key(fmt.Sprintf("%d", i))
		if err != nil {
			return err
		}

		if !strings.HasPrefix(strKey, prefix) {
			continue
		}

		value, err := GetItem(strKey)
		if err != nil {
			return err
		}

		err = f(strKey, value)
		if err != nil {
			return err
		}
	}

	return nil
}
