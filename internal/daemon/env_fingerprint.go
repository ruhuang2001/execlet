package daemon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

// In-memory stores use a process key; durable stores retain their own private
// key. Fingerprints alone do not reveal low-entropy environment secrets.
// Reading the key or all process memory defeats that protection.
var invocationFingerprintKey = func() [32]byte {
	var key [32]byte
	_, _ = rand.Read(key[:]) // crypto/rand.Read always fills the buffer or terminates.
	return key
}()

// keyedFingerprint hashes a length-framed sequence, so neither separators
// inside values nor different entry boundaries can alias another invocation.
// The caller supplies deterministic ordering. No input strings are retained.
func keyedFingerprint(key [32]byte, parts []string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key[:])
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(parts)))
	_, _ = mac.Write(size[:])
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(part))
	}
	var digest [sha256.Size]byte
	mac.Sum(digest[:0])
	return digest
}
