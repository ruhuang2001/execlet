package daemon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

var invocationFingerprintKey = func() [32]byte {
	var key [32]byte
	_, _ = rand.Read(key[:])
	return key
}()

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
