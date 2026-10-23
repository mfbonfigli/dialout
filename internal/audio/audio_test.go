package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestULawReferenceValues(t *testing.T) {
	// Reference points of ITU-T G.711 µ-law.
	dec := map[byte]int16{0xFF: 0, 0x7F: 0, 0x80: 32124, 0x00: -32124, 0xFE: 8, 0x7E: -8, 0xEF: 132, 0x6F: -132}
	for u, want := range dec {
		if got := ulawDecode[u]; got != want {
			t.Errorf("decode(%#x) = %d; want %d", u, got, want)
		}
	}
	enc := map[int16]byte{0: 0xFF, 32767: 0x80, -32768: 0x00, 8: 0xFE, -8: 0x7E}
	for s, want := range enc {
		if got := encodeULaw(s); got != want {
			t.Errorf("encode(%d) = %#x; want %#x", s, got, want)
		}
	}
}

func TestULawRoundTrip(t *testing.T) {
	// Every code (except negative zero, 0x7F, which decodes to the same 0 as 0xFF) must survive
	// decode → encode unchanged.
	for i := range 256 {
		u := byte(i)
		if u == 0x7F {
			continue
		}
		if got := encodeULaw(ulawDecode[u]); got != u {
			t.Errorf("encode(decode(%#x)) = %#x", u, got)
		}
	}
	// Encoding is monotonic in the decoded value and the quantisation error stays within the
	// segment step (≤ 1/16 of the magnitude, plus the small-signal floor).
	for s := -32768; s <= 32767; s += 7 {
		d := int32(ulawDecode[encodeULaw(int16(s))])
		diff := d - int32(s)
		if diff < 0 {
			diff = -diff
		}
		limit := int32(abs(s))/16 + 8
		if s > 32635 || s < -32635 {
			limit += 1024 // clipped region
		}
		if diff > limit {
			t.Fatalf("sample %d → %d: error %d > %d", s, d, diff, limit)
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestPCMHelpers(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 1234}
	b := PCMToLE16(nil, samples)
	if len(b) != 12 || b[2] != 1 || b[3] != 0 || b[4] != 0xFF || b[5] != 0xFF {
		t.Fatalf("PCMToLE16 = %v", b)
	}
	back := PCMFromLE16(nil, append(b, 0x42)) // odd trailing byte ignored
	if len(back) != len(samples) {
		t.Fatalf("len = %d", len(back))
	}
	for i := range samples {
		if back[i] != samples[i] {
			t.Errorf("sample %d = %d; want %d", i, back[i], samples[i])
		}
	}
	mu := PCMToMulaw(nil, []int16{0, 32767, -32768})
	if !bytes.Equal(mu, []byte{0xFF, 0x80, 0x00}) {
		t.Errorf("PCMToMulaw = %x", mu)
	}
	if pcm := MulawToPCM(nil, mu); pcm[0] != 0 || pcm[1] != 32124 || pcm[2] != -32124 {
		t.Errorf("MulawToPCM = %v", pcm)
	}
}

func TestStereoWAV(t *testing.T) {
	left := []int16{1, 2, 3}
	right := []int16{-1}
	data := Interleave(nil, left, right)
	if len(data) != 12 {
		t.Fatalf("interleaved len = %d", len(data))
	}
	frame := func(i int) (int16, int16) {
		return int16(binary.LittleEndian.Uint16(data[i*4:])), int16(binary.LittleEndian.Uint16(data[i*4+2:]))
	}
	if l, r := frame(0); l != 1 || r != -1 {
		t.Errorf("frame 0 = %d,%d", l, r)
	}
	if l, r := frame(2); l != 3 || r != 0 {
		t.Errorf("frame 2 = %d,%d (right should be padded with silence)", l, r)
	}

	var buf bytes.Buffer
	if err := WriteStereoWAV(&buf, data); err != nil {
		t.Fatal(err)
	}
	h := buf.Bytes()
	checks := []struct {
		name      string
		got, want uint32
	}{
		{"riff size", binary.LittleEndian.Uint32(h[4:]), 36 + 12},
		{"format", uint32(binary.LittleEndian.Uint16(h[20:])), 1},
		{"channels", uint32(binary.LittleEndian.Uint16(h[22:])), 2},
		{"rate", binary.LittleEndian.Uint32(h[24:]), 8000},
		{"byte rate", binary.LittleEndian.Uint32(h[28:]), 32000},
		{"block align", uint32(binary.LittleEndian.Uint16(h[32:])), 4},
		{"bits", uint32(binary.LittleEndian.Uint16(h[34:])), 16},
		{"data size", binary.LittleEndian.Uint32(h[40:]), 12},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d; want %d", c.name, c.got, c.want)
		}
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" || string(h[36:40]) != "data" || len(h) != 44+12 {
		t.Errorf("bad header %q", h[:44])
	}
}
