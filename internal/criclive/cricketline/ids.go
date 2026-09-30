package cricketline

import (
	"errors"
	"fmt"
	"strings"
)

// CricketLineApi keys every entity with a short alphanumeric string ("14F7" a
// match, "JG" a team, "2MT" a series). The feed pipeline keys by int64 end to
// end — Mongo documents and indexes, realtime topics, the frontend's team-id
// parsing — so keys are mapped into a reserved int64 range with a reversible
// bijective base-62 encoding. Bijective digits (1..62) keep "0A" and "A"
// distinct, which a plain positional encoding would not.
const (
	keyAlphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	keyBase      = int64(len(keyAlphabet))
	maxKeyLength = 8

	// IDNamespace sits far above any CricLive id, so the two feeds never
	// collide in the shared collections, and with the eight-character cap every
	// encoded id stays below 2^53: it survives JSON parsing in the browser.
	IDNamespace int64 = 1_000_000_000_000_000
)

// maxEncoded is the value of the largest key: eight copies of the last digit.
var maxEncoded = func() int64 {
	var value int64
	for i := 0; i < maxKeyLength; i++ {
		value = value*keyBase + keyBase
	}
	return value
}()

// IDMin and IDMax bound the ids this feed owns, as a half-open range.
var (
	IDMin = IDNamespace + 1
	IDMax = IDNamespace + maxEncoded + 1
)

var ErrInvalidKey = errors.New("invalid CricketLine key")

// EncodeKey maps a CricketLine key into the feed's int64 range.
func EncodeKey(key string) (int64, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > maxKeyLength {
		return 0, fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	var value int64
	for _, r := range key {
		digit := strings.IndexRune(keyAlphabet, r)
		if digit < 0 {
			return 0, fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
		value = value*keyBase + int64(digit) + 1
	}
	return IDNamespace + value, nil
}

// DecodeKey recovers the CricketLine key an id was encoded from.
func DecodeKey(id int64) (string, error) {
	if !IsFeedID(id) {
		return "", fmt.Errorf("%w: id %d is outside the CricketLine range", ErrInvalidKey, id)
	}
	value := id - IDNamespace
	out := make([]byte, 0, maxKeyLength)
	for value > 0 {
		digit := (value - 1) % keyBase
		out = append(out, keyAlphabet[digit])
		value = (value - digit - 1) / keyBase
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

// IsFeedID reports whether id belongs to this feed.
func IsFeedID(id int64) bool {
	return id >= IDMin && id < IDMax
}

// encodeOrZero is EncodeKey for optional keys: an invalid or absent key yields
// 0, which every consumer already treats as "missing".
func encodeOrZero(key string) int64 {
	id, err := EncodeKey(key)
	if err != nil {
		return 0
	}
	return id
}
