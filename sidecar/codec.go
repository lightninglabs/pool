package sidecar

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	// checksumLen is the number of bytes we add as a checksum. We'll take
	// this many bytes out of the full SHA256 hash and add it to the string
	// encoded version of the ticket as an additional integrity check.
	checksumLen = 4
)

var (
	// defaultStringEncoding is the default encoding algorithm we use to
	// encode the gzipped binary data as a string.
	defaultStringEncoding = base64.URLEncoding

	// sidecarPrefix is a specially prepared prefix that will spell out
	// "sidecar" when encoded as base64. It will cause all our sidecar
	// tickets to be easily recognizable with a human readable part.
	sidecarPrefix = []byte{0xb2, 0x27, 0x5e, 0x71, 0xaa, 0xc0}
)

// EncodeToString serializes and encodes the ticket as an URL safe string that
// contains a human readable prefix and a checksum.
func EncodeToString(t *Ticket) (string, error) {
	var buf bytes.Buffer

	// Add a prefix to make the ticket easily recognizable by human eyes.
	if _, err := buf.Write(sidecarPrefix); err != nil {
		return "", err
	}

	// Serialize the raw ticket itself.
	if err := SerializeTicket(&buf, t); err != nil {
		return "", err
	}

	// Create a checksum (SHA256) of all the bytes so far (prefix + raw
	// ticket) and add 4 bytes of that to the string encoded version. This
	// will prevent the ticket from appearing valid even if parts of it were
	// not copy/pasted correctly.
	hash := sha256.New()
	_, _ = hash.Write(buf.Bytes())
	checksum := hash.Sum(nil)[0:4]
	if _, err := buf.Write(checksum); err != nil {
		return "", err
	}

	return defaultStringEncoding.EncodeToString(buf.Bytes()), nil
}

// DecodeString decodes and then deserializes the given sidecar ticket string.
func DecodeString(s string) (*Ticket, error) {
	rawBytes, err := defaultStringEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}

	if len(rawBytes) < len(sidecarPrefix)+checksumLen {
		return nil, fmt.Errorf("not a sidecar ticket, invalid length")
	}

	totalLength := len(rawBytes)
	prefixLength := len(sidecarPrefix)
	payloadLength := totalLength - prefixLength - checksumLen

	prefix := rawBytes[0:prefixLength]
	payload := rawBytes[prefixLength : prefixLength+payloadLength]
	checksum := rawBytes[totalLength-checksumLen : totalLength]

	// Make sure the prefix matches our static prefix.
	if !bytes.Equal(prefix, sidecarPrefix) {
		return nil, fmt.Errorf("not a sicecar ticket, invalid prefix "+
			"%x", prefix)
	}

	// Calculate the SHA256 sum of the prefix and payload and compare it to
	// the checksum we also expect to be in the full string serialized
	// ticket.
	hash := sha256.New()
	_, _ = hash.Write(prefix)
	_, _ = hash.Write(payload)
	calculatedChecksum := hash.Sum(nil)[0:4]
	if !bytes.Equal(checksum, calculatedChecksum) {
		return nil, fmt.Errorf("invalid sidecar ticket, checksum " +
			"mismatch")
	}

	// Everything's fine, let's decode the actual payload.
	return DeserializeTicket(bytes.NewReader(payload))
}
