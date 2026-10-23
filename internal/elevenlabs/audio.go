package elevenlabs

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mfbonfigli/dialout/internal/audio"
)

// format is an agent audio format as ElevenLabs names it: ulaw_8000 or pcm_<rate> (16-bit
// little-endian). The phone line is 8 kHz, so ulaw_8000 (or pcm_8000) on both sides of the agent
// avoids any resampling; the other rates are resampled here.
type format struct {
	ulaw bool
	rate int
}

var pcmRates = map[int]bool{8000: true, 16000: true, 22050: true, 24000: true, 44100: true, 48000: true}

func parseFormat(s string) (format, error) {
	if s == "ulaw_8000" {
		return format{ulaw: true, rate: audio.SampleRate}, nil
	}
	if r, err := strconv.Atoi(strings.TrimPrefix(s, "pcm_")); err == nil && strings.HasPrefix(s, "pcm_") && pcmRates[r] {
		return format{rate: r}, nil
	}
	return format{}, fmt.Errorf("elevenlabs: unsupported audio format %q", s)
}

// encode turns 8 kHz PCM from the line into the agent's input format.
func (f format) encode(pcm []int16) []byte {
	pcm = resample(pcm, audio.SampleRate, f.rate)
	if f.ulaw {
		return audio.PCMToMulaw(nil, pcm)
	}
	return audio.PCMToLE16(nil, pcm)
}

// decode turns the agent's output into 8 kHz PCM for the line.
func (f format) decode(b []byte) []int16 {
	var pcm []int16
	if f.ulaw {
		pcm = audio.MulawToPCM(nil, b)
	} else {
		pcm = audio.PCMFromLE16(nil, b)
	}
	return resample(pcm, f.rate, audio.SampleRate)
}

// resample converts between sample rates by linear interpolation, averaging over the ratio first
// when the rate goes down so higher frequencies don't fold back. Good enough for speech on a phone
// line; chunks are resampled independently.
func resample(in []int16, from, to int) []int16 {
	if from == to || len(in) == 0 {
		return in
	}
	if from > to {
		in = movingAverage(in, (from+to-1)/to)
	}
	n := len(in) * to / from
	out := make([]int16, n)
	for i := range out {
		pos := float64(i) * float64(from) / float64(to)
		j := int(pos)
		a := float64(in[j])
		b := a
		if j+1 < len(in) {
			b = float64(in[j+1])
		}
		out[i] = int16(a + (b-a)*(pos-float64(j)))
	}
	return out
}

func movingAverage(in []int16, w int) []int16 {
	if w <= 1 {
		return in
	}
	out := make([]int16, len(in))
	var sum int64
	for i, v := range in {
		sum += int64(v)
		if i >= w {
			sum -= int64(in[i-w])
		}
		out[i] = int16(sum / int64(min(i+1, w)))
	}
	return out
}
