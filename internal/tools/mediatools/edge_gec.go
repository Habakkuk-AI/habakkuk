package mediatools

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"strings"
	"time"
)

// The read-aloud service's request token, version 2: "<CS64>-<DIGEST>".
//
// The input is the Windows file time (100 ns ticks since 1601) rounded down
// to five minutes, in decimal, followed by the client token. DIGEST is a
// SHA-256 variant: its first block is resumed from a fixed state at round
// 14, with the first seven input bytes in words 14 and 15 and a precomputed
// message schedule; the remaining bytes are padded as if 57 bytes preceded
// them and run through standard SHA-256 blocks. CS64 is a 64-bit checksum of
// the input keyed by the digest's first eight bytes. Both are upper-case
// hex; CS64 is two little-endian 32-bit words.

var (
	gecIV    = [8]uint32{0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A, 0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19}
	gecState = [8]uint32{0xFD9FBEA2, 0x91243CE7, 0xBE499094, 0xCD2E1E0A, 0x6C006F49, 0x04BD35AB, 0xFEB0A7E4, 0x03EFFBD2}
	gecSched = [14]uint32{0xEB24BF9E, 0x1558691F, 0xA0C09EE2, 0x0CFA32BA, 0xA622EE8C, 0xD1DD9491, 0xC0DD25A0,
		0xF9E70AB6, 0xF0C31573, 0x7058CE4E, 0x7188E606, 0x00E5AB33, 0xD258B060, 0x72766564}
	sha256K = [64]uint32{
		0x428A2F98, 0x71374491, 0xB5C0FBCF, 0xE9B5DBA5, 0x3956C25B, 0x59F111F1, 0x923F82A4, 0xAB1C5ED5,
		0xD807AA98, 0x12835B01, 0x243185BE, 0x550C7DC3, 0x72BE5D74, 0x80DEB1FE, 0x9BDC06A7, 0xC19BF174,
		0xE49B69C1, 0xEFBE4786, 0x0FC19DC6, 0x240CA1CC, 0x2DE92C6F, 0x4A7484AA, 0x5CB0A9DC, 0x76F988DA,
		0x983E5152, 0xA831C66D, 0xB00327C8, 0xBF597FC7, 0xC6E00BF3, 0xD5A79147, 0x06CA6351, 0x14292967,
		0x27B70A85, 0x2E1B2138, 0x4D2C6DFC, 0x53380D13, 0x650A7354, 0x766A0ABB, 0x81C2C92E, 0x92722C85,
		0xA2BFE8A1, 0xA81A664B, 0xC24B8B70, 0xC76C51A3, 0xD192E819, 0xD6990624, 0xF40E3585, 0x106AA070,
		0x19A4C116, 0x1E376C08, 0x2748774C, 0x34B0BCB5, 0x391C0CB3, 0x4ED8AA4A, 0x5B9CCA4F, 0x682E6FF3,
		0x748F82EE, 0x78A5636F, 0x84C87814, 0x8CC70208, 0x90BEFFFA, 0xA4506CEB, 0xBEF9A3F7, 0xC67178F2,
	}
	cs64Coef = [14][2]uint32{
		{0xE82E91B1, 0xB32700D0}, {0x44DE24C6, 0x88DFF51D}, {0x3BEB65BC, 0x29514038}, {0xC89C5933, 0xB328CBDF},
		{0x8D1EC211, 0xD405B4D5}, {0x314EF5D5, 0x26E55F3C}, {0xE92491B1, 0x607801C6}, {0xF0A5B9D5, 0x82B2C1FB},
		{0x06064CF3, 0xFA51A3DA}, {0xDA79FA60, 0x207CC464}, {0xBED20560, 0x3C3C74F3}, {0x89F9C9F8, 0xD2D14B61},
		{0x35FA38C6, 0xBB04A2C1}, {0xCB49E3C6, 0xC5626167},
	}
)

const (
	gecK14     = 0xC9DC06A7 // replaces K[14] in the first block
	gecSigma0X = 0x059C0B80
	gecSigma1X = 0x000B92C0
)

func sigma0(x uint32) uint32 { return bits.RotateLeft32(x, -7) ^ bits.RotateLeft32(x, -18) ^ x>>3 }
func sigma1(x uint32) uint32 { return bits.RotateLeft32(x, -17) ^ bits.RotateLeft32(x, -19) ^ x>>10 }

func expand(w *[64]uint32, from int) {
	for i := from; i < 64; i++ {
		w[i] = sigma1(w[i-2]) + w[i-7] + sigma0(w[i-15]) + w[i-16]
	}
}

// rounds runs SHA-256 rounds first..63 from state and returns the working
// registers (without the feed-forward).
func rounds(state [8]uint32, w *[64]uint32, k *[64]uint32, first int) [8]uint32 {
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	for i := first; i < 64; i++ {
		s1 := bits.RotateLeft32(e, -6) ^ bits.RotateLeft32(e, -11) ^ bits.RotateLeft32(e, -25)
		t1 := h + s1 + (g ^ (e & (f ^ g))) + k[i] + w[i]
		s0 := bits.RotateLeft32(a, -2) ^ bits.RotateLeft32(a, -13) ^ bits.RotateLeft32(a, -22)
		t2 := s0 + ((a & b) | (c & (a | b)))
		a, b, c, d, e, f, g, h = t1+t2, a, b, c, d+t1, e, f, g
	}
	return [8]uint32{a, b, c, d, e, f, g, h}
}

// gecDigest is the token's SHA-256 variant over data (at least 7 bytes).
func gecDigest(data []byte) [32]byte {
	var w [64]uint32
	w[14] = uint32(data[0])<<16 | uint32(data[1])<<8 | uint32(data[2])
	w[15] = binary.BigEndian.Uint32(data[3:7])
	for i := 16; i < 30; i++ {
		v := gecSched[i-16]
		if i-2 >= 14 {
			s := sigma1(w[i-2])
			if i == 16 {
				s ^= gecSigma1X
			}
			v += s
		}
		if i-7 >= 14 {
			v += w[i-7]
		}
		if i-15 >= 14 {
			s := sigma0(w[i-15])
			if i == 29 {
				s ^= gecSigma0X
			}
			v += s
		}
		w[i] = v
	}
	w[30] = sigma1(w[28]) + w[23] + sigma0(w[15]) + w[14] + (gecK14 - sha256K[14])
	expand(&w, 31)
	k := sha256K
	k[14] = gecK14
	work := rounds(gecState, &w, &k, 14)
	var state [8]uint32
	for i := range state {
		state[i] = gecIV[i] + work[i]
	}

	length := uint64(57 + len(data))
	padded := append([]byte{}, data[7:]...)
	padded = append(padded, 0x80)
	padded = append(padded, make([]byte, (55-int(length%64)+64)%64)...)
	padded = binary.BigEndian.AppendUint64(padded, length*8)
	for off := 0; off < len(padded); off += 64 {
		var bw [64]uint32
		for i := range 16 {
			bw[i] = binary.BigEndian.Uint32(padded[off+4*i:])
		}
		expand(&bw, 16)
		work := rounds(state, &bw, &sha256K, 0)
		for i := range state {
			state[i] += work[i]
		}
	}
	var out [32]byte
	for i, v := range state {
		binary.BigEndian.PutUint32(out[4*i:], v)
	}
	return out
}

// gecCS64 is the token's checksum of data keyed by the digest.
func gecCS64(data []byte, digest [32]byte) (uint32, uint32) {
	rol16 := func(v uint32) uint32 { return bits.RotateLeft32(v, 16) }
	keyA := binary.LittleEndian.Uint32(digest[0:4]) | 1
	keyB := binary.LittleEndian.Uint32(digest[4:8]) | 1
	factors := [2]uint32{keyA * 0x82893933, keyB * 0x82893933}
	var terms, offs []uint32
	for i, c := range cs64Coef {
		terms = append(terms, c[0]*factors[i%2])
		offs = append(offs, c[1])
	}
	in := append([]byte{0}, data...)
	for len(in)%4 != 0 {
		in = append(in, 0)
	}
	var words []uint32
	for i := 0; i < len(in); i += 4 {
		words = append(words, binary.LittleEndian.Uint32(in[i:]))
	}
	if len(words)%2 == 1 {
		words = append(words, 0)
	}
	for i, w := range words {
		key, mul := keyA, uint32(0xEF0569FB)
		if i%2 == 1 {
			key, mul = keyB, 0xC31713DB
		}
		term, off := w*key, rol16(w)*mul
		if i == 0 {
			term += 0xF2F90B1A * factors[0]
			off += 0x0B1A0000
		}
		terms = append(terms, term)
		offs = append(offs, off)
	}
	var swapAcc, swapLast, state, acc uint32
	for i := 0; i < len(terms); i += 2 {
		x, y := terms[i], terms[i+1]
		q := rol16(offs[i]+x) * 0x76F50F34
		swapAcc += rol16(q)*0xC3EFEA97 + q
		q = rol16(offs[i+1]+y) * 0x1881CE3C
		swapLast = rol16(q)*0x35BD1EC9 + q
		swapAcc += swapLast

		state = state*keyA + x
		state = rol16(state) * 0xCF98B111
		state = rol16(state) * 0x87085B9F
		state = rol16(state) * 0x12CEB96D
		state = rol16(state) * 0x257E1D83
		acc += state
		t := state*keyB + y
		t = rol16(t) * 0xA27416F5
		t = rol16(t) * 0xD38396FF
		state = rol16(rol16(t)*0x7C932B89) * 0xBFA49F69
		acc += state
	}
	return swapLast ^ state, swapAcc ^ acc
}

// edgeGEC is the Sec-MS-GEC token for a moment.
func edgeGEC(now time.Time) string {
	ticks := now.Unix() + winEpoch
	ticks -= ticks % 300
	data := fmt.Appendf(nil, "%d%s", ticks*10_000_000, edgeToken)
	digest := gecDigest(data)
	a, b := gecCS64(data, digest)
	cs := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, a), b)
	return strings.ToUpper(hex.EncodeToString(cs)) + "-" + strings.ToUpper(hex.EncodeToString(digest[:]))
}
