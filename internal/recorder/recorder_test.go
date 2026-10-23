package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func fill(n int, v int16) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestPlayout(t *testing.T) {
	var p playout
	p.queued("a", fill(1600, 1), 0)
	p.queued("b", fill(1600, 2), 0)
	// The first echo comes back a little after the audio ended: placed by the echo.
	if got := p.played("a", 1760); len(got) != 1 || got[0].at != 160 {
		t.Fatalf("a placed at %+v", got)
	}
	// Continuous playback stays contiguous.
	if got := p.played("b", 3360); len(got) != 1 || got[0].at != 1760 {
		t.Fatalf("b placed at %+v", got)
	}
	// A late echo after silence: it can't have started before it was queued.
	p.queued("c", fill(800, 3), 8000)
	if got := p.played("c", 9000); len(got) != 1 || got[0].at != 8200 {
		t.Fatalf("c placed at %+v", got)
	}
	// A lost echo: both chunks are placed, one after the other.
	p.queued("d", fill(800, 4), 10000)
	p.queued("e", fill(800, 5), 10000)
	if got := p.played("e", 11700); len(got) != 2 || got[0].at != 10000 || got[1].at != 10900 {
		t.Fatalf("d, e placed at %+v", got)
	}
	// Barge-in: the head is cut where the clear happened, the rest is dropped.
	p.queued("f", fill(1600, 6), 12000)
	p.queued("g", fill(1600, 7), 12000)
	got := p.cleared(12500)
	if len(got) != 1 || got[0].at != 12000 || len(got[0].pcm) != 500 || got[0].pcm[0] != 6 {
		t.Fatalf("clear placed %+v", got)
	}
	if got := p.played("g", 13000); got != nil {
		t.Errorf("echo of cleared audio placed %+v", got)
	}
	if got := p.cleared(14000); got != nil {
		t.Errorf("clear with nothing queued placed %+v", got)
	}
	p.queued("h", fill(100, 8), 15000)
	if got := p.cleared(15000); got != nil {
		t.Errorf("clear before anything played placed %+v", got)
	}
}

// memStore is an in-memory Uploader.
type memStore struct {
	mu    sync.Mutex
	objs  map[string][]byte
	types map[string]string
	order []string
	fail  bool
}

func newStore() *memStore { return &memStore{objs: map[string][]byte{}, types: map[string]string{}} }

func (m *memStore) Put(_ context.Context, key, contentType string, body io.ReadSeeker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("s3 unavailable")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.objs[key] = b
	m.types[key] = contentType
	m.order = append(m.order, key)
	return nil
}

func (m *memStore) get(t *testing.T, key string) []byte {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		t.Fatalf("%s not uploaded; have %v", key, m.keys())
	}
	return b
}

func (m *memStore) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objs[key]
	return ok
}

func (m *memStore) keys() []string {
	var k []string
	for key := range m.objs {
		k = append(k, key)
	}
	slices.Sort(k)
	return k
}

func (m *memStore) waitFor(t *testing.T, keys ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		m.mu.Lock()
		missing := ""
		for _, k := range keys {
			if _, ok := m.objs[k]; !ok {
				missing = k
			}
		}
		m.mu.Unlock()
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never uploaded", missing)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// decodeWAV checks a stereo 8 kHz 16-bit WAV and returns its channels.
func decodeWAV(t *testing.T, b []byte) (left, right []int16) {
	t.Helper()
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[36:40]) != "data" {
		t.Fatalf("not a WAV file (%d bytes)", len(b))
	}
	le := binary.LittleEndian
	if le.Uint16(b[22:]) != 2 || le.Uint32(b[24:]) != 8000 || le.Uint16(b[34:]) != 16 {
		t.Fatalf("format: %d channels, %d Hz, %d bits", le.Uint16(b[22:]), le.Uint32(b[24:]), le.Uint16(b[34:]))
	}
	if int(le.Uint32(b[40:])) != len(b)-44 || int(le.Uint32(b[4:])) != len(b)-8 {
		t.Fatalf("sizes in header don't match the file")
	}
	for i := 44; i+3 < len(b); i += 4 {
		left = append(left, int16(le.Uint16(b[i:])))
		right = append(right, int16(le.Uint16(b[i+2:])))
	}
	return left, right
}

// checkRange asserts that s[from:to] is all v.
func checkRange(t *testing.T, name string, s []int16, from, to int, v int16) {
	t.Helper()
	for i := from; i < to; i++ {
		if s[i] != v {
			t.Errorf("%s[%d] = %d, want %d (range %d-%d)", name, i, s[i], v, from, to)
			return
		}
	}
}

func testConfig(t *testing.T, store *memStore) Config {
	return Config{
		SpoolDir: filepath.Join(t.TempDir(), "spool"),
		Segment:  time.Second,
		Audio:    true,
		Uploader: store,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

var started = time.Date(2026, 10, 21, 23, 30, 0, 0, time.FixedZone("CEST", 2*3600))

// feed sends the callee's audio frame by frame, as Twilio does: 20 ms at a time.
func feed(r *Recorder, fromMs, toMs int64, v int16) {
	for ms := fromMs; ms < toMs; ms += 20 {
		r.Callee(ms, fill(160, v))
	}
}

func TestRecording(t *testing.T) {
	store := newStore()
	cfg := testConfig(t, store)
	r, err := Start(cfg, "call-1", started, []byte(`{"status":"in-progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "calls/2026/10/21/call-1/" // UTC date
	if r.Prefix() != prefix {
		t.Fatalf("prefix = %s", r.Prefix())
	}

	feed(r, 0, 1000, 100) // clock 8000
	r.AgentQueued("a1", fill(1600, 200))
	feed(r, 1000, 1200, 100) // clock 9600: a1 has played
	r.AgentPlayed("a1")
	if err := r.Transcript("agent", "Buongiorno"); err != nil {
		t.Fatal(err)
	}
	feed(r, 1200, 2000, 100) // clock 16000
	r.AgentQueued("a2", fill(1600, 300))
	r.AgentQueued("a3", fill(1600, 400))
	feed(r, 2000, 2200, 100) // clock 17600
	r.AgentPlayed("a2")
	feed(r, 2200, 2240, 100) // clock 17920: the callee talks over the agent
	r.AgentCleared()
	r.AgentPlayed("a3") // Twilio's echo for the cleared audio
	if err := r.Transcript("callee", "Scusi, un attimo"); err != nil {
		t.Fatal(err)
	}
	feed(r, 2240, 5000, 100) // clock 40000

	// Segments 1 and 2 ended more than 3 s ago: uploaded during the call, with the transcript.
	store.waitFor(t, prefix+"segments/0001.wav", prefix+"segments/0002.wav")
	if store.has(prefix + "segments/0003.wav") {
		t.Error("segment 3 uploaded before it was due")
	}
	if got := string(store.get(t, prefix+"transcript.jsonl")); !strings.Contains(got, `{"t":1.2,"speaker":"agent","text":"Buongiorno"}`) {
		t.Errorf("transcript during the call = %q", got)
	}

	res, err := r.Finish(context.Background(), []byte(`{"status":"completed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Prefix != prefix || res.RecordingKey != prefix+"recording.wav" || res.Segments != 5 || res.Duration != 5*time.Second {
		t.Errorf("result = %+v", res)
	}
	want := []string{
		prefix + "meta.json", prefix + "recording.wav",
		prefix + "segments/0001.wav", prefix + "segments/0002.wav", prefix + "segments/0003.wav",
		prefix + "segments/0004.wav", prefix + "segments/0005.wav", prefix + "transcript.jsonl",
	}
	if got := store.keys(); !slices.Equal(got, want) {
		t.Fatalf("uploaded:\n%v\nwant\n%v", got, want)
	}
	if last := store.order[len(store.order)-1]; last != prefix+"meta.json" {
		t.Errorf("last upload = %s, want meta.json", last)
	}
	if got := string(store.get(t, prefix+"meta.json")); got != `{"status":"completed"}` {
		t.Errorf("meta = %s", got)
	}
	if store.types[prefix+"recording.wav"] != "audio/wav" || store.types[prefix+"meta.json"] != "application/json" {
		t.Errorf("content types = %v", store.types)
	}
	tr := string(store.get(t, prefix+"transcript.jsonl"))
	if tr != "{\"t\":1.2,\"speaker\":\"agent\",\"text\":\"Buongiorno\"}\n{\"t\":2.2,\"speaker\":\"callee\",\"text\":\"Scusi, un attimo\"}\n" {
		t.Errorf("transcript = %q", tr)
	}

	left, right := decodeWAV(t, store.get(t, prefix+"recording.wav"))
	if len(left) != 40000 {
		t.Fatalf("recording has %d samples, want 40000", len(left))
	}
	checkRange(t, "left", left, 0, 40000, 100)
	checkRange(t, "right", right, 0, 8000, 0)
	checkRange(t, "right", right, 8000, 9600, 200)   // a1, when it played
	checkRange(t, "right", right, 9600, 16000, 0)    //
	checkRange(t, "right", right, 16000, 17600, 300) // a2
	checkRange(t, "right", right, 17600, 17920, 400) // a3 until the clear
	checkRange(t, "right", right, 17920, 40000, 0)   // the rest of a3 was never heard
	seg2l, seg2r := decodeWAV(t, store.get(t, prefix+"segments/0002.wav"))
	if !slices.Equal(seg2l, left[8000:16000]) || !slices.Equal(seg2r, right[8000:16000]) {
		t.Error("segment 2 differs from the recording")
	}

	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "call-1")); !os.IsNotExist(err) {
		t.Errorf("spool not removed: %v", err)
	}
	// Nothing happens after Finish.
	r.Callee(5000, fill(160, 1))
	if err := r.Transcript("agent", "x"); err == nil {
		t.Error("Transcript after Finish accepted")
	}
	if _, err := r.Finish(context.Background(), []byte(`{}`)); err == nil {
		t.Error("second Finish accepted")
	}
}

func TestTranscriptOnly(t *testing.T) {
	store := newStore()
	cfg := testConfig(t, store)
	cfg.Audio = false
	r, err := Start(cfg, "call-0", started, []byte(`{"status":"in-progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	prefix := r.Prefix()
	feed(r, 0, 1000, 100)
	r.AgentQueued("a1", fill(1600, 200))
	feed(r, 1000, 1200, 100)
	r.AgentPlayed("a1")
	if err := r.Transcript("agent", "Buongiorno"); err != nil {
		t.Fatal(err)
	}
	r.AgentCleared()
	feed(r, 1200, 5000, 100)

	// The transcript is still uploaded during the call, on the segments' schedule.
	store.waitFor(t, prefix+"transcript.jsonl")
	res, err := r.Finish(context.Background(), []byte(`{"status":"completed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.RecordingKey != "" || res.Segments != 0 {
		t.Errorf("result = %+v", res)
	}
	if got, want := store.keys(), []string{prefix + "meta.json", prefix + "transcript.jsonl"}; !slices.Equal(got, want) {
		t.Errorf("uploaded %v, want %v", got, want)
	}
	if tr := string(store.get(t, prefix+"transcript.jsonl")); tr != "{\"t\":1.2,\"speaker\":\"agent\",\"text\":\"Buongiorno\"}\n" {
		t.Errorf("transcript = %q", tr)
	}
	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "call-0")); !os.IsNotExist(err) {
		t.Errorf("spool not removed: %v", err)
	}
}

func TestEndOfCallAudio(t *testing.T) {
	store := newStore()
	r, err := Start(testConfig(t, store), "call-2", started, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	feed(r, 0, 1500, 1) // clock 12000
	// The goodbye plays after the callee's last frame we saw, then audio that never played.
	r.AgentQueued("bye", fill(800, 2))
	r.AgentPlayed("bye")
	r.AgentQueued("unheard", fill(800, 3))
	// Agent audio for a segment closed long ago is dropped, not written back in time.
	res, err := r.Finish(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	left, right := decodeWAV(t, store.get(t, res.RecordingKey))
	if len(left) != 12800 || res.Segments != 2 || res.Duration != 1600*time.Millisecond {
		t.Fatalf("%d samples, result %+v", len(left), res)
	}
	checkRange(t, "left", left, 0, 12000, 1)
	checkRange(t, "left", left, 12000, 12800, 0)
	checkRange(t, "right", right, 0, 12000, 0)
	checkRange(t, "right", right, 12000, 12800, 2)
}

func TestLateAgentAudio(t *testing.T) {
	store := newStore()
	r, err := Start(testConfig(t, store), "call-3", started, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r.AgentQueued("long", fill(40000, 5)) // 5 s queued at once at clock 0
	feed(r, 0, 6000, 1)                   // clock 48000; segments 1-3 closed
	r.AgentPlayed("long")                 // placed at 8000: its first two seconds are too late
	res, err := r.Finish(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_, right := decodeWAV(t, store.get(t, res.RecordingKey))
	checkRange(t, "right", right, 0, 24000, 0)
	checkRange(t, "right", right, 24000, 48000, 5)
}

func TestUploadFailureThenRecover(t *testing.T) {
	store := newStore()
	store.fail = true
	cfg := testConfig(t, store)
	r, err := Start(cfg, "call-4", started, []byte(`{"status":"in-progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	feed(r, 0, 4500, 1) // segment 1 closed during the call, its upload fails
	if _, err := r.Finish(context.Background(), []byte(`{"status":"completed"}`)); err == nil {
		t.Fatal("Finish succeeded with S3 down")
	}
	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "call-4", "segments", "0005.wav")); err != nil {
		t.Fatalf("spool not kept: %v", err)
	}

	store.fail = false
	var fixed []string
	recs, err := Recover(context.Background(), cfg, func(id string, meta []byte) []byte {
		fixed = append(fixed, id+" "+string(meta))
		return meta // a completed call stays completed
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Err != nil || recs[0].Result.Segments != 5 || recs[0].Result.Duration != 4500*time.Millisecond {
		t.Fatalf("recovered = %+v", recs)
	}
	if !slices.Equal(fixed, []string{`call-4 {"status":"completed"}`}) {
		t.Errorf("fix called with %v", fixed)
	}
	prefix := "calls/2026/10/21/call-4/"
	left, _ := decodeWAV(t, store.get(t, prefix+"recording.wav"))
	if len(left) != 36000 {
		t.Errorf("recording has %d samples", len(left))
	}
	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "call-4")); !os.IsNotExist(err) {
		t.Errorf("spool not removed after recovery: %v", err)
	}
}

// crash stops a recorder the way a crash would: no Finish, the open segment lost.
func (r *Recorder) crash() {
	r.mu.Lock()
	r.finished = true
	r.mu.Unlock()
	close(r.work)
	<-r.done
	r.tmu.Lock()
	r.tfile.Close()
	r.tfile = nil
	r.tmu.Unlock()
}

func TestRecoverAfterCrash(t *testing.T) {
	store := newStore()
	cfg := testConfig(t, store)
	r, err := Start(cfg, "call-5", started, []byte(`{"status":"in-progress","to":"Marco"}`))
	if err != nil {
		t.Fatal(err)
	}
	feed(r, 0, 1000, 1)
	if err := r.Transcript("callee", "Pronto?"); err != nil {
		t.Fatal(err)
	}
	feed(r, 1000, 5500, 1) // segments 1-2 closed; 3-6 still open in memory
	prefix := "calls/2026/10/21/call-5/"
	store.waitFor(t, prefix+"segments/0002.wav")
	r.crash()
	// A write the crash interrupted, and a directory that isn't a call.
	os.WriteFile(filepath.Join(cfg.SpoolDir, "call-5", "segments", "0003.wav.tmp"), []byte("RIFF"), 0o600)
	os.Mkdir(filepath.Join(cfg.SpoolDir, "stray"), 0o700)

	after := newStore()
	cfg.Uploader = after
	recs, err := Recover(context.Background(), cfg, func(id string, meta []byte) []byte {
		return []byte(strings.Replace(string(meta), `"in-progress"`, `"interrupted"`, 1))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].CallID != "call-5" || recs[0].Err != nil {
		t.Fatalf("recovered = %+v", recs)
	}
	if got := string(after.get(t, prefix+"meta.json")); got != `{"status":"interrupted","to":"Marco"}` {
		t.Errorf("meta = %s", got)
	}
	if got := string(after.get(t, prefix+"transcript.jsonl")); got != "{\"t\":1,\"speaker\":\"callee\",\"text\":\"Pronto?\"}\n" {
		t.Errorf("transcript = %q", got)
	}
	left, _ := decodeWAV(t, after.get(t, prefix+"recording.wav"))
	if len(left) != 16000 {
		t.Errorf("recovered recording has %d samples, want the 2 closed segments", len(left))
	}
	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "stray")); err != nil {
		t.Errorf("stray directory touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.SpoolDir, "call-5")); !os.IsNotExist(err) {
		t.Errorf("spool not removed: %v", err)
	}
}

func TestAssembleFillsMissingSegments(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, segmentsDir), 0o700)
	write := func(name string, n int, v int16) {
		var pcm []byte
		for range n {
			pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
			pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v))
		}
		var wav []byte
		wav = append(wav, []byte("RIFF")...)
		wav = append(wav, make([]byte, 40)...)
		wav = append(wav, pcm...)
		os.WriteFile(filepath.Join(dir, segmentsDir, name), wav, 0o600)
	}
	write("0001.wav", 100, 1)
	write("0003.wav", 50, 3) // 0002 was lost
	segs, err := spooledSegments(dir)
	if err != nil || len(segs) != 2 {
		t.Fatalf("segments = %+v, %v", segs, err)
	}
	out := filepath.Join(dir, "rec.wav")
	n, err := assemble(out, segs, 100)
	if err != nil || n != 250*4 {
		t.Fatalf("assemble = %d, %v", n, err)
	}
	b, _ := os.ReadFile(out)
	left, _ := decodeWAV(t, b)
	checkRange(t, "left", left, 0, 100, 1)
	checkRange(t, "left", left, 100, 200, 0)
	checkRange(t, "left", left, 200, 250, 3)
}

func TestStartValidation(t *testing.T) {
	store := newStore()
	cfg := testConfig(t, store)
	for _, id := range []string{"", "../etc", "a/b", strings.Repeat("x", 65)} {
		if _, err := Start(cfg, id, started, []byte(`{}`)); err == nil {
			t.Errorf("call id %q accepted", id)
		}
	}
	if _, err := Start(cfg, "ok", started, []byte(`not json`)); err == nil {
		t.Error("invalid meta accepted")
	}
	r, err := Start(cfg, "dup", started, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Start(cfg, "dup", started, []byte(`{}`)); err == nil {
		t.Error("second recorder for the same call accepted")
	}
	r.Finish(context.Background(), []byte(`{}`))

	bad := cfg
	bad.Segment = 1500 * time.Millisecond
	if _, err := Start(bad, "x", started, []byte(`{}`)); err == nil {
		t.Error("fractional segment length accepted")
	}
	if recs, err := Recover(context.Background(), Config{SpoolDir: filepath.Join(t.TempDir(), "none"), Uploader: store}, nil); err != nil || recs != nil {
		t.Errorf("Recover on a missing spool = %v, %v", recs, err)
	}
}

func TestDisabled(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool")
	r, err := Start(Config{Disabled: true, SpoolDir: spool}, "call-x", started, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	feed(r, 0, 5000, 100)
	r.AgentQueued("a1", fill(1600, 200))
	r.AgentPlayed("a1")
	r.AgentCleared()
	if err := r.Transcript("agent", "Buongiorno"); err != nil || r.SetMeta([]byte(`{}`)) != nil {
		t.Errorf("transcript or meta: %v", err)
	}
	if res, err := r.Finish(context.Background(), []byte(`{}`)); err != nil || res != (Result{}) {
		t.Errorf("Finish = %+v, %v", res, err)
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Errorf("spool created: %v", err)
	}
	if recs, err := Recover(context.Background(), Config{Disabled: true}, nil); err != nil || recs != nil {
		t.Errorf("Recover = %v, %v", recs, err)
	}
}
