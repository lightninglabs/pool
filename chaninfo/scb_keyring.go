package chaninfo

import "github.com/lightningnetwork/lnd/keychain"

// scbEncryptionKeyLocator is the key locator used to obtain the key with which
// a channel backup is encrypted with.
var scbEncryptionKeyLocator = keychain.KeyLocator{
	Family: keychain.KeyFamilyBaseEncryption,
	Index:  0,
}

// scbKeyRing is a wrapper struct we'll use for decrypting retrieve channel
// backups. This keyring implementation will always return the node's channel
// backup encryption key descriptor, but it must be obtained before creation.
type scbKeyRing struct {
	encryptionKey keychain.KeyDescriptor
}

// A compile-time constraint to ensure scbKeyRing satisfies keychain.KeyRing.
var _ keychain.KeyRing = (*scbKeyRing)(nil)

// DeriveNextKey retruns the node's channel backup encryption key descriptor.
func (k *scbKeyRing) DeriveNextKey(
	keychain.KeyFamily) (keychain.KeyDescriptor, error) {

	return k.encryptionKey, nil
}

// DeriveKey retruns the node's channel backup encryption key descriptor.
func (k *scbKeyRing) DeriveKey(
	keychain.KeyLocator) (keychain.KeyDescriptor, error) {

	return k.encryptionKey, nil
}
