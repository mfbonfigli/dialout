package recorder

// playout places the agent's audio on the timeline when it was actually played. The
// bridge sends Sonic's audio to Twilio in chunks, each followed by a mark; Twilio echoes the mark
// once the chunk has finished playing. A clear (barge-in) cuts the chunk that is playing and drops
// the queued ones, so the recording matches what the callee heard. Times are sample positions on
// the stream timeline.
type playout struct {
	queue   []chunk
	lastEnd int64 // where the previous placed chunk ended
}

type chunk struct {
	mark     string
	pcm      []int16
	queuedAt int64
}

// placement is agent audio to write at a position.
type placement struct {
	at  int64
	pcm []int16
}

func (p *playout) queued(mark string, pcm []int16, now int64) {
	p.queue = append(p.queue, chunk{mark: mark, pcm: pcm, queuedAt: now})
}

// played handles a mark echo at now: the chunk with that mark (and any before it whose echo got
// lost) has played. A chunk can't start before it was sent or before the previous one ended; the
// echoed one ended about now. Unknown marks, such as echoes for audio dropped by a clear, are
// ignored.
func (p *playout) played(mark string, now int64) []placement {
	n := -1
	for i, c := range p.queue {
		if c.mark == mark {
			n = i
			break
		}
	}
	if n < 0 {
		return nil
	}
	var out []placement
	for i, c := range p.queue[:n+1] {
		start := max(p.lastEnd, c.queuedAt)
		if i == n {
			start = max(start, now-int64(len(c.pcm)))
		}
		out = append(out, placement{at: start, pcm: c.pcm})
		p.lastEnd = start + int64(len(c.pcm))
	}
	p.queue = p.queue[n+1:]
	return out
}

// cleared handles a clear sent at now: the chunk at the head of the queue was playing and is cut
// there; everything queued after it is dropped.
func (p *playout) cleared(now int64) []placement {
	if len(p.queue) == 0 {
		return nil
	}
	head := p.queue[0]
	p.queue = nil
	start := max(p.lastEnd, head.queuedAt)
	n := min(max(now-start, 0), int64(len(head.pcm)))
	if n == 0 {
		return nil
	}
	p.lastEnd = start + n
	return []placement{{at: start, pcm: head.pcm[:n]}}
}

// drop forgets audio that never played (the call ended first).
func (p *playout) drop() { p.queue = nil }
