package daemon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

// The key lives only for this process, just like the execution records. A
// fingerprint alone cannot be used to guess low-entropy environment secrets.
// This does not protect secrets from an attacker who can read all process memory.
var invocationFingerprintKey = func() [32]byte {
	var key [32]byte
	_, _ = rand.Read(key[:]) // crypto/rand.Read always fills the buffer or terminates.
	return key
}()

// invocationFingerprint hashes a length-framed sequence, so neither separators
// inside values nor different entry boundaries can alias another invocation.
// The caller supplies deterministic ordering. No input strings are retained.
func invocationFingerprint(parts []string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, invocationFingerprintKey[:])
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

func envFingerprint(req ExecuteRequest) [sha256.Size]byte {
	return invocationFingerprint(req.envPairs())
}
