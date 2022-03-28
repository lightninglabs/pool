package sidecar

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/base58"
)

const (
	// checksumLen is the number of bytes we add as a checksum. We'll take
	// this many bytes out of the full SHA256 hash and add it to the string
	// encoded version of the ticket as an additional integrity check.
	checksumLen = 4

	// sidecarPrefix will serve as our "human readable" prefix to make the
	// strings easily identifiable.
	sidecarPrefix = "sidecar"
)

var (
	// encodingVersion is the current encoding version used so each ticket
	// will have a predictable character after the sidecar prefix.
	encodingVersion = []byte{0}
)

// EncodeToString serializes and encodes the ticket as an URL safe string that
// contains a human readable prefix and a checksum.
func EncodeToString(t *Ticket) (string, error) {
	var (
		checksumBuf bytes.Buffer
		encodeBuf   bytes.Buffer
	)

	// First, we'll write the sidecar prefix, as well as the serialized
	// ticket into the buffer that we'll use to generate the checksum. We
	// do this as we won't encode the checksum using base58.
	if _, err := checksumBuf.Write([]byte(sidecarPrefix)); err != nil {
		return "", err
	}
	if _, err := checksumBuf.Write(encodingVersion); err != nil {
		return "", err
	}
	if err := SerializeTicket(&checksumBuf, t); err != nil {
		return "", err
	}

	// We'll now hash our full checksum buffer to generate the checksum
	// that we'll tack onto the end of the ticket.
	checksum := sha256.Sum256(checksumBuf.Bytes())

	// Now that we have the checksum, we'll write the raw ticket and the
	// checksum to the buffer that we'll use for encoding.
	//
	// Similar to Bitcoin, we'll add a version so the prefix of the string
	// is always the same.
	if _, err := encodeBuf.Write(encodingVersion); err != nil {
		return "", err
	}
	if err := SerializeTicket(&encodeBuf, t); err != nil {
		return "", err
	}
	if _, err := encodeBuf.Write(checksum[:checksumLen]); err != nil {
		return "", err
	}

	// The final ticket is the sidecar encoding (non base58 encoded) with
	// the base58 encoded ticket following it.
	baseTicket := base58.Encode(encodeBuf.Bytes())
	return sidecarPrefix + baseTicket, nil
}

// DecodeString decodes and then deserializes the given sidecar ticket string.
func DecodeString(s string) (*Ticket, error) {
	prefixLength := len(sidecarPrefix)
	if len(s) < prefixLength {
		return nil, fmt.Errorf("string contains invalid prefix")
	}

	// First, we'll decode the set of raw bytes from the base58 encoding,
	// snipping off our custom prefix before decoding.
	rawBytes := base58.Decode(s[prefixLength:])

	// We'll also ensure that the length passes basic sanity checks to not
	// trip up the slicing logic below.
	expectedLen := len(encodingVersion) + checksumLen
	if len(rawBytes) < expectedLen {
		return nil, fmt.Errorf("not a sidecar ticket, invalid "+
			"length: %v vs %v", len(rawBytes), expectedLen)
	}

	// We'll ensure that the sidecar prefix matches exactly (of the string
	// version), as otherwise the checksum check won't pass anyway.
	encodedPrefix := s[:prefixLength]
	if encodedPrefix != sidecarPrefix {
		return nil, fmt.Errorf("not a sidecar ticket, invalid prefix "+
			"%x", encodedPrefix)
	}

	// Now that we know the prefix passes, we'll move onto verifying the
	// checksum.
	totalLength := len(rawBytes)
	versionLen := len(encodingVersion)
	payloadLength := totalLength - checksumLen - len(encodingVersion)

	// Extract the payload (between the prefix and the checksum), from the
	// checksum itself (everything after the payload).
	payload := rawBytes[versionLen : versionLen+payloadLength]
	checksum := rawBytes[totalLength-checksumLen : totalLength]

	// Calculate the SHA256 sum of the human readable prefix and payload
	// and compare it to the checksum we also expect to be in the full
	// string serialized ticket.
	hash := sha256.New()
	_, _ = hash.Write([]byte(sidecarPrefix))
	_, _ = hash.Write(encodingVersion)
	_, _ = hash.Write(payload)
	calculatedChecksum := hash.Sum(nil)[:checksumLen]
	if !bytes.Equal(checksum, calculatedChecksum) {
		return nil, fmt.Errorf("invalid sidecar ticket, checksum " +
			"mismatch")
	}

	// Everything's fine, let's decode the actual payload.
	return DeserializeTicket(bytes.NewReader(payload))
}
