// Package audio converts between Twilio's G.711 µ-law and Nova Sonic's 16-bit LPCM (both 8 kHz
// mono) and writes the stereo WAV recordings.
package audio

const (
	SampleRate   = 8000
	FrameMillis  = 20
	FrameSamples = SampleRate * FrameMillis / 1000 // 160 samples per 20 ms frame
)

var ulawDecode [256]int16

func init() {
	for i := range ulawDecode {
		ulawDecode[i] = decodeULaw(byte(i))
	}
}

// decodeULaw is the ITU-T G.711 µ-law expansion.
func decodeULaw(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F
	sample := ((int32(mantissa) << 3) + 0x84) << exponent
	sample -= 0x84
	if sign != 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// encodeULaw is the ITU-T G.711 µ-law compression (bias 0x84, clip at 32635).
func encodeULaw(s int16) byte {
	const bias, clip = 0x84, 32635
	x := int32(s)
	sign := byte(0)
	if x < 0 {
		x = -x
		sign = 0x80
	}
	if x > clip {
		x = clip
	}
	x += bias
	exponent := byte(7)
	for mask := int32(0x4000); x&mask == 0 && exponent > 0; mask >>= 1 {
		exponent--
	}
	mantissa := byte((x >> (exponent + 3)) & 0x0F)
	return ^(sign | exponent<<4 | mantissa)
}

// MulawToPCM decodes µ-law bytes into 16-bit samples, appending to dst.
func MulawToPCM(dst []int16, src []byte) []int16 {
	for _, b := range src {
		dst = append(dst, ulawDecode[b])
	}
	return dst
}

// PCMToMulaw encodes 16-bit samples into µ-law bytes, appending to dst.
func PCMToMulaw(dst []byte, src []int16) []byte {
	for _, s := range src {
		dst = append(dst, encodeULaw(s))
	}
	return dst
}

// PCMFromLE16 parses little-endian 16-bit LPCM bytes (Nova Sonic's audio format) into samples.
// A trailing odd byte is ignored.
func PCMFromLE16(dst []int16, b []byte) []int16 {
	for i := 0; i+1 < len(b); i += 2 {
		dst = append(dst, int16(uint16(b[i])|uint16(b[i+1])<<8))
	}
	return dst
}

// PCMToLE16 serialises samples as little-endian 16-bit LPCM.
func PCMToLE16(dst []byte, s []int16) []byte {
	for _, v := range s {
		dst = append(dst, byte(v), byte(uint16(v)>>8))
	}
	return dst
}
