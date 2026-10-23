package audio

import (
	"encoding/binary"
	"io"
)

// WAVHeaderSize is the size of the canonical 44-byte PCM WAV header.
const WAVHeaderSize = 44

// WAVHeader returns a canonical PCM 16-bit WAV header for dataBytes of audio.
func WAVHeader(channels, sampleRate, dataBytes int) []byte {
	h := make([]byte, WAVHeaderSize)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+dataBytes))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:], uint32(sampleRate*channels*2)) // byte rate
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*2))            // block align
	binary.LittleEndian.PutUint16(h[34:], 16)                            // bits per sample
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(dataBytes))
	return h
}

// Interleave combines the callee (left) and agent (right) channels into stereo frames. The shorter
// channel is padded with silence so both stay aligned.
func Interleave(dst []byte, left, right []int16) []byte {
	n := max(len(left), len(right))
	for i := range n {
		var l, r int16
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		dst = append(dst, byte(l), byte(uint16(l)>>8), byte(r), byte(uint16(r)>>8))
	}
	return dst
}

// WriteStereoWAV writes a complete stereo 8 kHz WAV file from interleaved 16-bit PCM bytes.
func WriteStereoWAV(w io.Writer, interleaved []byte) error {
	if _, err := w.Write(WAVHeader(2, SampleRate, len(interleaved))); err != nil {
		return err
	}
	_, err := w.Write(interleaved)
	return err
}
