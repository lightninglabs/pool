package account

import (
	"crypto/sha256"
	"testing"

	"github.com/btcsuite/btcd/btcec"
)

var (
	privKeyBytes    = sha256.Sum256([]byte("brainwallet_private_key"))
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), privKeyBytes[:])
	clientNonce     = sha256.Sum256([]byte("nonce1"))
	serverNonce     = sha256.Sum256([]byte("nonce2"))
)

// TestAuthHandshake tests that the 3-way handshake between client and server is
// cryptographically correct.
func TestAuthHandshake(t *testing.T) {
	// Step 1: The client commits to the key they want to authenticate for
	// and sends the commitment to the server.
	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey.SerializeCompressed())
	commitHash := CommitAccount(pubKeyBytes, clientNonce)

	// Step 2: The server receives the commitment and adds its nonce to it
	// to create the challenge which it sends back to the client.
	authChallenge := AuthChallenge(commitHash, serverNonce)

	// Step 3a: The client signs the commitment and the challenge to get
	// a signature over authHash and sends that to the server, together with
	// the nonce they chose and the public key they used to sign.
	authHash := AuthHash(commitHash, authChallenge)
	sig, err := privKey.Sign(authHash[:])
	if err != nil {
		t.Fatalf("error signing auth hash: %v", err)
	}

	// Step 3b: The server receives the signature, the client's nonce and
	// public key. The server then reconstructs the full authHash and
	// verifies the signature was in fact made over said hash.
	authHashServer := AuthHash(
		CommitAccount(pubKeyBytes, clientNonce),
		AuthChallenge(
			CommitAccount(pubKeyBytes, clientNonce), serverNonce,
		),
	)
	res := sig.Verify(authHashServer[:], pubKey)
	if !res {
		t.Fatalf("signature did not match reconstructed hash")
	}
}
