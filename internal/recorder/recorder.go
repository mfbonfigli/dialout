// Package recorder records a call: a JSONL transcript and the call's meta.json and,
// when Config.Audio is set, the audio as the callee heard it, a stereo 8 kHz WAV (left: callee,
// right: agent). The call is cut into segments that are spooled to disk and uploaded to S3 while
// it runs, each with the transcript so far. Finish assembles recording.wav; Recover finishes, at
// start-up, the calls a crash or a hibernation interrupted.
package recorder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/mfbonfigli/dialout/internal/audio"
)

const (
	DefaultSegment = 60 * time.Second
	// closeMargin is how long after its end a segment is closed, so agent audio placed late (when
	// its mark comes back) still lands in it.
	closeMargin = 3 * time.Second
	// maxTimeline bounds the recording, so a bogus timestamp can't make it allocate or close
	// segments without end. Calls last at most 5 minutes; a Sonic session at most 8.
	maxTimeline = 15 * time.Minute
	// uploadTimeout bounds each upload.
	uploadTimeout = 2 * time.Minute
)

// Object names inside a call's prefix.
const (
	MetaFile       = "meta.json"
	TranscriptFile = "transcript.jsonl"
	RecordingFile  = "recording.wav"
	segmentsDir    = "segments"
	stateFile      = "call.json"
)

// Uploader stores one object. S3 is the production implementation.
type Uploader interface {
	Put(ctx context.Context, key, contentType string, body io.ReadSeeker) error
}

// Config is shared by every call.
type Config struct {
	SpoolDir string        // a directory per call is created inside, e.g. /data/spool
	Segment  time.Duration // segment length, DefaultSegment when 0
	Audio    bool          // record the audio too; without it only the transcript and meta.json
	Disabled bool          // store nothing at all: no spool, no uploads; every method does nothing
	Uploader Uploader
	Log      *slog.Logger // slog.Default() when nil
}

func (c *Config) normalize() error {
	if c.SpoolDir == "" {
		return errors.New("recorder: no spool directory")
	}
	if c.Uploader == nil {
		return errors.New("recorder: no uploader")
	}
	if c.Segment == 0 {
		c.Segment = DefaultSegment
	}
	if c.Segment < time.Second || c.Segment%time.Second != 0 {
		return fmt.Errorf("recorder: segment length %v must be a whole number of seconds", c.Segment)
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	return nil
}

var callIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Prefix is where a call's objects go: calls/YYYY/MM/DD/<call_id>/, dated by the start in UTC.
func Prefix(callID string, started time.Time) string {
	return "calls/" + started.UTC().Format("2006/01/02") + "/" + callID + "/"
}

func segmentName(index int64) string { return fmt.Sprintf("%04d.wav", index+1) }

func samples(d time.Duration) int64 { return int64(d / time.Second * audio.SampleRate) }

// state is call.json in the spool: what Recover needs to finish the call.
type state struct {
	CallID         string    `json:"call_id"`
	Prefix         string    `json:"prefix"`
	Started        time.Time `json:"started"`
	SegmentSeconds int       `json:"segment_seconds"`
}

// Recorder records one call. Its methods are safe to call from the bridge's goroutines.
type Recorder struct {
	off        bool // Config.Disabled
	cfg        Config
	st         state
	dir        string
	segSamples int64

	mu         sync.Mutex
	clock      int64 // end of the latest callee audio: the stream's "now", in samples
	end        int64 // furthest sample written on either channel
	segs       map[int64]*segment
	nextClose  int64 // index of the first segment still open
	play       playout
	transcript []byte
	finished   bool
	lateDrops  int64 // samples that arrived for a segment already closed

	tmu   sync.Mutex // the transcript file
	tfile *os.File

	work     chan job
	done     chan struct{}
	uploaded map[string]bool // written by the worker; read by Finish once done is closed
}

type segment struct{ left, right []int16 }

type job struct {
	index      int64
	pcm        []byte // interleaved stereo
	transcript []byte // snapshot uploaded along with the segment
}

// Start begins recording a call: it creates the call's spool directory with meta (the call's
// meta.json so far) and starts the upload worker. Audio times are relative to the start of the
// Twilio stream.
func Start(cfg Config, callID string, started time.Time, meta []byte) (*Recorder, error) {
	if cfg.Disabled {
		return &Recorder{off: true}, nil
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if !callIDRE.MatchString(callID) {
		return nil, fmt.Errorf("recorder: invalid call id %q", callID)
	}
	if !json.Valid(meta) {
		return nil, errors.New("recorder: meta is not valid JSON")
	}
	if err := os.MkdirAll(cfg.SpoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	dir := filepath.Join(cfg.SpoolDir, callID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("recorder: %w", err) // also when the call is already spooled
	}
	if err := os.Mkdir(filepath.Join(dir, segmentsDir), 0o700); err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	st := state{
		CallID:         callID,
		Prefix:         Prefix(callID, started),
		Started:        started.UTC(),
		SegmentSeconds: int(cfg.Segment / time.Second),
	}
	stb, _ := json.Marshal(st)
	if err := writeFileAtomic(filepath.Join(dir, stateFile), stb); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, MetaFile), meta); err != nil {
		return nil, err
	}
	tfile, err := os.OpenFile(filepath.Join(dir, TranscriptFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	r := &Recorder{
		cfg:        cfg,
		st:         st,
		dir:        dir,
		segSamples: samples(cfg.Segment),
		segs:       map[int64]*segment{},
		tfile:      tfile,
		work:       make(chan job, 32),
		done:       make(chan struct{}),
		uploaded:   map[string]bool{},
	}
	go r.worker()
	return r, nil
}

// Prefix is where this call's objects go.
func (r *Recorder) Prefix() string { return r.st.Prefix }

// Callee records the callee's audio from one Twilio media message, placed by its timestamp
// (milliseconds since the stream started). It also advances the stream clock that places
// everything else.
func (r *Recorder) Callee(tsMillis int64, pcm []int16) {
	if r.off {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return
	}
	at := tsMillis * audio.SampleRate / 1000
	if at < 0 || at >= samples(maxTimeline) {
		r.lateDrops += int64(len(pcm))
		return
	}
	if r.cfg.Audio {
		r.put(0, at, pcm)
	}
	r.clock = max(r.clock, at+int64(len(pcm)))
	r.closeDue()
}

// AgentQueued records agent audio sent to Twilio and followed by a mark with this name. It is
// placed on the timeline once AgentPlayed reports the mark back.
func (r *Recorder) AgentQueued(mark string, pcm []int16) {
	if r.off {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || !r.cfg.Audio {
		return
	}
	r.play.queued(mark, slices.Clone(pcm), r.clock)
}

// AgentPlayed handles Twilio's echo of a mark: the audio before it has played.
func (r *Recorder) AgentPlayed(mark string) {
	if r.off {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || !r.cfg.Audio {
		return
	}
	for _, p := range r.play.played(mark, r.clock) {
		r.put(1, p.at, p.pcm)
	}
}

// AgentCleared handles a clear sent to Twilio (barge-in): the audio playing is cut now and the
// audio still queued is dropped, as the callee never heard it.
func (r *Recorder) AgentCleared() {
	if r.off {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished || !r.cfg.Audio {
		return
	}
	for _, p := range r.play.cleared(r.clock) {
		r.put(1, p.at, p.pcm)
	}
}

// Transcript appends a final transcript line ({"t", "speaker", "text"}), timed by the stream clock
// when it arrives.
func (r *Recorder) Transcript(speaker, text string) error {
	if r.off {
		return nil
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return errors.New("recorder: finished")
	}
	t := math.Round(float64(r.clock)/audio.SampleRate*10) / 10
	line, _ := json.Marshal(struct {
		T       float64 `json:"t"`
		Speaker string  `json:"speaker"`
		Text    string  `json:"text"`
	}{t, speaker, text})
	line = append(line, '\n')
	r.transcript = append(r.transcript, line...)
	r.mu.Unlock()

	// The disk write happens outside mu, so it never holds up the audio.
	r.tmu.Lock()
	defer r.tmu.Unlock()
	if r.tfile == nil {
		return errors.New("recorder: finished")
	}
	if _, err := r.tfile.Write(line); err != nil {
		return fmt.Errorf("recorder: transcript: %w", err)
	}
	return r.tfile.Sync()
}

// SetMeta replaces the call's meta.json in the spool, e.g. once the call is answered.
func (r *Recorder) SetMeta(meta []byte) error {
	if r.off {
		return nil
	}
	if !json.Valid(meta) {
		return errors.New("recorder: meta is not valid JSON")
	}
	return writeFileAtomic(filepath.Join(r.dir, MetaFile), meta)
}

// put writes samples to a channel (0 left, 1 right) from position at, across segments. Samples for
// a segment already closed are dropped and counted.
func (r *Recorder) put(ch int, at int64, pcm []int16) {
	for len(pcm) > 0 {
		idx := at / r.segSamples
		off := at - idx*r.segSamples
		n := min(int64(len(pcm)), r.segSamples-off)
		if idx < r.nextClose || at+n > samples(maxTimeline) {
			r.lateDrops += n
		} else {
			s := r.segs[idx]
			if s == nil {
				s = &segment{left: make([]int16, r.segSamples), right: make([]int16, r.segSamples)}
				r.segs[idx] = s
			}
			dst := s.left
			if ch == 1 {
				dst = s.right
			}
			copy(dst[off:off+n], pcm[:n])
			r.end = max(r.end, at+n)
		}
		at += n
		pcm = pcm[n:]
	}
}

// closeDue closes the segments that ended more than closeMargin ago on the stream clock.
func (r *Recorder) closeDue() {
	for (r.nextClose+1)*r.segSamples+samples(closeMargin) <= r.clock {
		r.closeSegment(r.nextClose, r.segSamples)
		r.nextClose++
	}
}

// closeSegment hands the first n samples of a segment to the worker, with the transcript so far.
func (r *Recorder) closeSegment(idx, n int64) {
	j := job{index: idx, transcript: slices.Clone(r.transcript)}
	if r.cfg.Audio {
		s := r.segs[idx]
		delete(r.segs, idx)
		var left, right []int16
		if s != nil {
			left, right = s.left[:n], s.right[:n]
		} else {
			left = make([]int16, n) // nothing was heard: silence
		}
		j.pcm = audio.Interleave(make([]byte, 0, n*4), left, right)
	}
	r.work <- j
}

// worker spools each closed segment and uploads it with the transcript so far.
func (r *Recorder) worker() {
	defer close(r.done)
	for j := range r.work {
		if !r.cfg.Audio {
			r.upload(r.st.Prefix+TranscriptFile, "application/x-ndjson", bytes.NewReader(j.transcript))
			continue
		}
		name := segmentName(j.index)
		data := append(audio.WAVHeader(2, audio.SampleRate, len(j.pcm)), j.pcm...)
		if err := writeFileAtomic(filepath.Join(r.dir, segmentsDir, name), data); err != nil {
			r.cfg.Log.Error("recorder: spooling segment", "call_id", r.st.CallID, "segment", name, "err", err)
		}
		key := r.st.Prefix + segmentsDir + "/" + name
		if r.upload(key, "audio/wav", bytes.NewReader(data)) {
			r.uploaded[key] = true
		}
		r.upload(r.st.Prefix+TranscriptFile, "application/x-ndjson", bytes.NewReader(j.transcript))
	}
}

func (r *Recorder) upload(key, contentType string, body io.ReadSeeker) bool {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	if err := r.cfg.Uploader.Put(ctx, key, contentType, body); err != nil {
		r.cfg.Log.Warn("recorder: upload failed, retried at the end", "call_id", r.st.CallID, "key", key, "err", err)
		return false
	}
	return true
}

// Result describes a finished recording.
type Result struct {
	Prefix       string
	RecordingKey string // empty when no audio was recorded
	Duration     time.Duration
	Segments     int
}

// Finish ends the recording. It closes the last segment (agent audio that never played is
// dropped), replaces meta.json with meta, assembles recording.wav from the spool and uploads what
// isn't uploaded yet, meta.json last. On success the call's spool directory is removed; on error
// it is kept for Recover. The other methods do nothing after Finish.
func (r *Recorder) Finish(ctx context.Context, meta []byte) (Result, error) {
	if r.off {
		return Result{}, nil
	}
	if !json.Valid(meta) {
		return Result{}, errors.New("recorder: meta is not valid JSON")
	}
	r.mu.Lock()
	if r.finished {
		r.mu.Unlock()
		return Result{}, errors.New("recorder: already finished")
	}
	r.finished = true
	r.play.drop()
	end := max(r.clock, r.end)
	for idx := r.nextClose; idx*r.segSamples < end; idx++ {
		r.closeSegment(idx, min(r.segSamples, end-idx*r.segSamples))
	}
	late := r.lateDrops
	r.mu.Unlock()
	close(r.work)
	<-r.done

	r.tmu.Lock()
	err := r.tfile.Close()
	r.tfile = nil
	r.tmu.Unlock()
	if err != nil {
		r.cfg.Log.Error("recorder: closing transcript", "call_id", r.st.CallID, "err", err)
	}
	if late > 0 {
		r.cfg.Log.Warn("recorder: audio arrived too late for its segment", "call_id", r.st.CallID,
			"dropped", time.Duration(late)*time.Second/audio.SampleRate)
	}
	if err := r.SetMeta(meta); err != nil {
		return Result{}, err
	}
	return complete(ctx, r.cfg, r.dir, r.st, r.uploaded)
}

// complete assembles recording.wav from the spooled segments, uploads what isn't uploaded yet
// (segments, transcript, recording, then meta.json, so its presence in S3 means the call is
// complete) and removes the spool directory.
func complete(ctx context.Context, cfg Config, dir string, st state, uploaded map[string]bool) (Result, error) {
	res := Result{Prefix: st.Prefix}
	segs, err := spooledSegments(dir)
	if err != nil {
		return res, err
	}
	res.Segments = len(segs)

	var errs []error
	put := func(key, contentType, path string) {
		if uploaded[key] {
			return
		}
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err == nil {
			err = cfg.Uploader.Put(ctx, key, contentType, f)
			f.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("recorder: uploading %s: %w", key, err))
		}
	}
	for _, s := range segs {
		put(st.Prefix+segmentsDir+"/"+s.name, "audio/wav", s.path)
	}
	put(st.Prefix+TranscriptFile, "application/x-ndjson", filepath.Join(dir, TranscriptFile))
	if len(segs) > 0 {
		rec := filepath.Join(dir, RecordingFile)
		n, err := assemble(rec, segs, samples(time.Duration(st.SegmentSeconds)*time.Second))
		if err != nil {
			return res, err
		}
		res.Duration = time.Duration(n/4) * time.Second / audio.SampleRate
		res.RecordingKey = st.Prefix + RecordingFile
		put(res.RecordingKey, "audio/wav", rec)
	}
	if len(errs) == 0 { // meta.json last
		put(st.Prefix+MetaFile, "application/json", filepath.Join(dir, MetaFile))
	}
	if len(errs) > 0 {
		return res, errors.Join(errs...)
	}
	if err := os.RemoveAll(dir); err != nil {
		cfg.Log.Warn("recorder: removing spool", "call_id", st.CallID, "err", err)
	}
	return res, nil
}

type spooled struct {
	index int64
	name  string
	path  string
}

var segmentRE = regexp.MustCompile(`^(\d{4,})\.wav$`)

// spooledSegments lists the complete segment files of a call, in order. Temporary files from an
// interrupted write are ignored.
func spooledSegments(dir string) ([]spooled, error) {
	entries, err := os.ReadDir(filepath.Join(dir, segmentsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	var out []spooled
	for _, e := range entries {
		m := segmentRE.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		var n int64
		fmt.Sscan(m[1], &n)
		if n < 1 {
			continue
		}
		out = append(out, spooled{index: n - 1, name: e.Name(), path: filepath.Join(dir, segmentsDir, e.Name())})
	}
	slices.SortFunc(out, func(a, b spooled) int { return int(a.index - b.index) })
	return out, nil
}

// assemble writes the whole call as one WAV from its segments, with silence where a segment is
// missing so the timeline stays right. It returns the size of the audio data in bytes.
func assemble(path string, segs []spooled, segSamples int64) (int64, error) {
	var total int64
	for i, s := range segs {
		fi, err := os.Stat(s.path)
		if err != nil {
			return 0, fmt.Errorf("recorder: %w", err)
		}
		if fi.Size() < audio.WAVHeaderSize {
			return 0, fmt.Errorf("recorder: segment %s is truncated", s.name)
		}
		gap := s.index
		if i > 0 {
			gap = s.index - segs[i-1].index - 1
		}
		total += gap*segSamples*4 + fi.Size() - audio.WAVHeaderSize
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("recorder: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(audio.WAVHeader(2, audio.SampleRate, int(total))); err != nil {
		return 0, fmt.Errorf("recorder: %w", err)
	}
	silence := make([]byte, segSamples*4)
	prev := int64(-1)
	for _, s := range segs {
		for range s.index - prev - 1 {
			if _, err := f.Write(silence); err != nil {
				return 0, fmt.Errorf("recorder: %w", err)
			}
		}
		prev = s.index
		if err := appendData(f, s.path); err != nil {
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("recorder: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("recorder: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, fmt.Errorf("recorder: %w", err)
	}
	return total, nil
}

// appendData copies the audio data of a segment file (without its header) to w.
func appendData(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(audio.WAVHeaderSize, io.SeekStart); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	return nil
}

// writeFileAtomic replaces path with data so that a crash leaves either the old or the new file.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("recorder: %w", err)
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes a rename in dir durable. Best effort: not every platform can sync a directory.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
