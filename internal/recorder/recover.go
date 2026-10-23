package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Recovered is a call Recover found in the spool.
type Recovered struct {
	CallID string
	Result Result
	Err    error // the upload failed: the spool is kept for the next start
}

// Recover finishes the calls left in the spool by a crash or a hibernation. The
// segments already closed are uploaded and assembled into recording.wav; the segment that was
// open is lost. fix gets each call's meta.json and returns the version to store, e.g. with the
// status set to interrupted; it is also where the server updates its history.
// Run Recover at start-up, before any call starts. A spool directory that isn't a call's is left
// alone and logged.
func Recover(ctx context.Context, cfg Config, fix func(callID string, meta []byte) []byte) ([]Recovered, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(cfg.SpoolDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}
	var out []Recovered
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(cfg.SpoolDir, e.Name())
		st, err := readState(dir)
		if err != nil {
			cfg.Log.Error("recorder: not a recoverable call, left in the spool", "dir", dir, "err", err)
			continue
		}
		removeTemp(dir)
		removeTemp(filepath.Join(dir, segmentsDir))

		meta, err := os.ReadFile(filepath.Join(dir, MetaFile))
		if err != nil || !json.Valid(meta) {
			meta = []byte("{}")
		}
		if fixed := fix(st.CallID, meta); json.Valid(fixed) {
			meta = fixed
		}
		rec := Recovered{CallID: st.CallID}
		if err := writeFileAtomic(filepath.Join(dir, MetaFile), meta); err != nil {
			rec.Err = err
		} else {
			rec.Result, rec.Err = complete(ctx, cfg, dir, st, nil)
		}
		if rec.Err != nil {
			cfg.Log.Error("recorder: recovering call", "call_id", st.CallID, "err", rec.Err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func readState(dir string) (state, error) {
	var st state
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if !callIDRE.MatchString(st.CallID) || st.CallID != filepath.Base(dir) || st.SegmentSeconds < 1 ||
		st.Prefix != Prefix(st.CallID, st.Started) {
		return st, errors.New("inconsistent " + stateFile)
	}
	return st, nil
}

// removeTemp deletes the leftovers of writes a crash interrupted.
func removeTemp(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmp") {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
