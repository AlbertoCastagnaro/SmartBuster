package profile

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// FaviconHash computes the Shodan-convention favicon hash: base64-encode
// the raw body (with a newline every 76 chars, matching Python's
// base64.encodebytes), then mmh3 32-bit hash the resulting text, returned
// as Go's signed int32 (spec §4.4).
func FaviconHash(body []byte) int32 {
	encoded := shodanBase64(body)
	return int32(murmur3_32([]byte(encoded), 0))
}

func shodanBase64(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		b.WriteString(enc[i:end])
		b.WriteByte('\n')
	}
	return b.String()
}

// applyFaviconSignal votes for the tech behind a known favicon hash (spec
// §4.4). A miss casts no vote — an unrecognized favicon says nothing.
func applyFaviconSignal(p *TargetProfile, rs *Ruleset, body []byte) {
	if len(body) == 0 {
		return
	}
	hash := strconv.FormatInt(int64(FaviconHash(body)), 10)
	for _, r := range rs.Favicons {
		if r.Hash == hash {
			p.vote(r.Tech, r.Category, LayerBackend, SrcFavicon, r.Confidence, "", r.ID)
		}
	}
}

// murmur3_32 is the standard 32-bit MurmurHash3 (seed as given).
func murmur3_32(data []byte, seed uint32) uint32 {
	const (
		c1 = 0xcc9e2d51
		c2 = 0x1b873593
	)
	h := seed
	length := len(data)
	nblocks := length / 4

	for i := 0; i < nblocks; i++ {
		k := uint32(data[i*4]) | uint32(data[i*4+1])<<8 | uint32(data[i*4+2])<<16 | uint32(data[i*4+3])<<24
		k *= c1
		k = (k << 15) | (k >> 17)
		k *= c2

		h ^= k
		h = (h << 13) | (h >> 19)
		h = h*5 + 0xe6546b64
	}

	tail := data[nblocks*4:]
	var k1 uint32
	switch len(tail) {
	case 3:
		k1 ^= uint32(tail[2]) << 16
		fallthrough
	case 2:
		k1 ^= uint32(tail[1]) << 8
		fallthrough
	case 1:
		k1 ^= uint32(tail[0])
		k1 *= c1
		k1 = (k1 << 15) | (k1 >> 17)
		k1 *= c2
		h ^= k1
	}

	h ^= uint32(length)
	h ^= h >> 16
	h *= 0x85ebca6b
	h ^= h >> 13
	h *= 0xc2b2ae35
	h ^= h >> 16
	return h
}
