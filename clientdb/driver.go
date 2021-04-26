// +build !wasm

package clientdb

const (
	dbDriver = "bdb"
)

func isExistingDB(dir, path string) (bool, error) {
	// If the database file does not exist yet, create its directory.
	if !fileExists(path) {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return false, err
		}

		return false, nil
	}

	return true, nil
}
