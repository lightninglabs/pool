package account

import (
	"crypto/sha256"
)

// concatAndHash writes two byte slices to a sha256 hash and returns the sum.
// The result is SHA256(a || b).
func concatAndHash(a, b []byte) [32]byte {
	var result [32]byte

	// Hash both elements together. The Write function of a hash never
	// returns an error so we can safely ignore the return values.
	h := sha256.New()
	_, _ = h.Write(a)
	_, _ = h.Write(b)
	copy(result[:], h.Sum(nil))
	return result
}

// CommitAccount creates the commitment hash that is used for the first part of
// the 3-way authentication handshake with the server. It returns the
// commitment hash which is SHA256(accountPubKey || nonce).
func CommitAccount(acctPubKey [33]byte, nonce [32]byte) [32]byte {
	return concatAndHash(acctPubKey[:], nonce[:])
}

// AuthChallenge creates the authentication challenge that is sent from the
// server as step 2 of the 3-way authentication handshake with the client. It
// returns the authentication challenge which is SHA256(commit_hash || nonce).
func AuthChallenge(commitHash [32]byte, nonce [32]byte) [32]byte {
	return concatAndHash(commitHash[:], nonce[:])
}

// AuthHash creates the authentication hash that is signed as part of step 3 of
// the 3-way authentication handshake. It returns the authentication hash which
// is SHA256(commit_hash || auth_challenge) which is equal to
// SHA256(SHA256(accountPubKey || nonce1) || SHA256(commit_hash || nonce2)).
func AuthHash(commitHash, challenge [32]byte) [32]byte {
	return concatAndHash(commitHash[:], challenge[:])
}
