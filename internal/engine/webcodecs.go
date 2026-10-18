package engine

// webcodecs.go: WebCodecs, Go side. The JavaScript API is platformjs/webcodecs.js; docs/webcodecs.md has the
// support matrix.
//
// Work happens off the JS thread. Every VideoDecoder, VideoEncoder, AudioDecoder and AudioEncoder owns a goroutine
// and a queue of jobs (decode, encode, flush, reset); the JS thread only appends to the queue. Results travel back as
// events on the VM's own event queue, so output callbacks run from the event loop, in order, like any other
// asynchronous browser callback. reset() bumps an epoch: queued jobs of an older epoch are dropped and the events
// of a job that was already running are ignored by the JS side.
//
// Pixels and samples never enter the JS heap: a VideoFrame or AudioData object is a handle into webcodecs_data.go.
// Encoded chunks come back as blobs the JS side takes with __swWc_take.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"modernc.org/quickjs"

	"github.com/sweefbrowser/sweef/internal/media"
)

type wcHost struct {
	j       *jsVM
	mu      sync.Mutex
	next    int64
	codecs  map[int64]*wcCodec
	owned   map[int64]bool
	blobs   map[int64][]byte
	blobSeq int64
	pending atomic.Int32
	closed  bool
}

func (j *jsVM) wcState() *wcHost {
	j.clMu.Lock()
	defer j.clMu.Unlock()
	if j.wch == nil {
		h := &wcHost{j: j, codecs: map[int64]*wcCodec{}, owned: map[int64]bool{}, blobs: map[int64][]byte{}}
		j.wch = h
		j.onClose(h.shutdown)
	}
	return j.wch
}

// wcLive reports queued or running codec work (the event loop keeps pumping while there is some).
func (j *jsVM) wcBusy() bool {
	j.clMu.Lock()
	h := j.wch
	j.clMu.Unlock()
	return h != nil && h.pending.Load() > 0
}

func (h *wcHost) own(id int64) {
	h.mu.Lock()
	h.owned[id] = true
	h.mu.Unlock()
}

func (h *wcHost) drop(id int64) {
	h.mu.Lock()
	had := h.owned[id]
	delete(h.owned, id)
	h.mu.Unlock()
	if !had {
		return
	}
	if v, ok := wcHandles.LoadAndDelete(id); ok {
		v.(*wcHandle).d.release()
	}
}

func (h *wcHost) shutdown() {
	h.mu.Lock()
	h.closed = true
	cs := h.codecs
	h.codecs = map[int64]*wcCodec{}
	ids := make([]int64, 0, len(h.owned))
	for id := range h.owned {
		ids = append(ids, id)
	}
	h.blobs = nil
	h.mu.Unlock()
	for _, c := range cs {
		c.close()
	}
	for _, id := range ids {
		h.drop(id)
	}
}

func (h *wcHost) push(data string) {
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	h.j.rt().push(asyncEvent{kind: "wc", data: data})
	if h.j.tab != nil {
		h.j.tab.wake()
	}
}

func (h *wcHost) putBlob(b []byte) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.blobSeq++
	if h.blobs != nil {
		h.blobs[h.blobSeq] = b
	}
	return h.blobSeq
}

// ---- errors / JSON ------------------------------------------------------------------------------------------

// wcErr is the error form natives return: a string "!Name: message" the JS side throws.
func wcErr(name, f string, a ...any) string { return "!" + name + ": " + fmt.Sprintf(f, a...) }

// splitErr turns the "Name:message" form used by the helpers into the native's "!Name: message".
func splitErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, ':'); i > 0 && !strings.ContainsAny(s[:i], " ") {
		return "!" + s[:i] + ": " + s[i+1:]
	}
	return "!Error: " + s
}

func hostErrOf(err error) error {
	s := err.Error()
	if i := strings.IndexByte(s, ':'); i > 0 && !strings.ContainsAny(s[:i], " ") {
		return fmt.Errorf("%s: %s", s[:i], s[i+1:])
	}
	return err
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (hd *wcHandle) info() map[string]any {
	d := hd.d
	if d.audio {
		return map[string]any{"h": hd.id, "f": d.format, "r": d.rate, "c": d.nch, "n": d.frames}
	}
	return map[string]any{"h": hd.id, "f": d.format, "cw": d.w, "ch": d.h, "vx": hd.vx, "vy": hd.vy, "vw": hd.vw, "vh": hd.vh, "m": d.matrix, "fr": d.full, "p": d.prim, "t": d.trc}
}

// ---- codecs -----------------------------------------------------------------------------------------------

type wcCfg struct {
	Codec   string  `json:"codec"`
	Desc    string  `json:"desc"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Rate    int     `json:"rate"`
	Ch      int     `json:"ch"`
	Bitrate int     `json:"bitrate"`
	Fps     float64 `json:"fps"`
	KeyInt  int     `json:"keyInt"`
	FrameUs int     `json:"frameUs"`
	App     string  `json:"app"`
	ADTS    bool    `json:"adts"`
}

func (c *wcCfg) desc() []byte {
	if c.Desc == "" {
		return nil
	}
	b, _ := base64.StdEncoding.DecodeString(c.Desc)
	return b
}

type wcJobType int

const (
	jobDecode wcJobType = iota
	jobEncV
	jobEncA
	jobFlush
	jobReset
)

type wcJob struct {
	typ     wcJobType
	epoch   int64
	data    []byte
	hnd     *wcHandle
	ts, dur int64
	hasDur  bool
	key     bool
	flush   int64
}

type wcCodec struct {
	h    *wcHost
	id   int64
	kind string // vd ad ve ae
	cfg  wcCfg

	mu     sync.Mutex
	cond   *sync.Cond
	q      []wcJob
	closed bool
	epoch  int64

	// owned by the goroutine
	vdec    *media.WCVideoDecoder
	adec    *media.WCAudioDecoder
	venc    *media.WCVP8Encoder
	aenc    *media.WCOpusEncoder
	sentCfg bool
	encW    int
	encH    int
	fifo    []float32
	fifoTS  int64
	fifoPos int64 // samples emitted since fifoTS
	fifoSet bool

	// H.264 hands out each chunk's picture in decode order; WebCodecs outputs presentation order. Frames wait in
	// reorder, sorted by timestamp, until more than the stream's reorder depth are waiting (the decoder reports it;
	// `depth` also grows by one whenever a frame arrives that should have come out before one already delivered).
	// HEVC's decoder reorders by itself and its pictures lag their chunks: they take the smallest outstanding
	// timestamp from tsq.
	reorder  []wcPending
	tsq      []wcPending
	depth    int
	lastEmit int64
	emitted  bool
}

type wcPending struct {
	info   map[string]any
	ts     int64
	dur    int64
	hasDur bool
}

func wcBuildSupport(kind string, cfg *wcCfg) (c *wcCodec, err error) {
	c = &wcCodec{kind: kind, cfg: *cfg}
	switch kind {
	case "vd":
		c.vdec, err = media.NewWCVideoDecoder(cfg.Codec, cfg.desc())
	case "ad":
		c.adec, err = media.NewWCAudioDecoder(cfg.Codec, cfg.desc(), cfg.Rate, cfg.Ch, cfg.ADTS)
	case "ve":
		if cfg.Codec != "vp8" {
			return nil, fmt.Errorf("NotSupportedError:no encoder for %q (this build encodes VP8 video)", cfg.Codec)
		}
		if cfg.W < 16 || cfg.H < 16 || cfg.W > 16383 || cfg.H > 16383 {
			return nil, fmt.Errorf("NotSupportedError:unsupported frame size %dx%d (VP8: 16 to 16383)", cfg.W, cfg.H)
		}
		c.venc, err = media.NewWCVP8Encoder(cfg.W, cfg.H, cfg.Bitrate, cfg.Fps, cfg.KeyInt)
		c.encW, c.encH = cfg.W, cfg.H
	case "ae":
		if cfg.Codec != "opus" {
			return nil, fmt.Errorf("NotSupportedError:no encoder for %q (this build encodes Opus audio)", cfg.Codec)
		}
		c.aenc, err = media.NewWCOpusEncoder(cfg.Rate, cfg.Ch, cfg.Bitrate, cfg.FrameUs, cfg.App)
	default:
		return nil, fmt.Errorf("TypeError:unknown codec kind")
	}
	if err != nil {
		return nil, fmt.Errorf("NotSupportedError:%v", err)
	}
	return c, nil
}

func (h *wcHost) newCodec(kind string, cfg *wcCfg) (int64, error) {
	c, err := wcBuildSupport(kind, cfg)
	if err != nil {
		return 0, err
	}
	c.h = h
	c.cond = sync.NewCond(&c.mu)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, fmt.Errorf("InvalidStateError:the document is closing")
	}
	h.next++
	c.id = h.next
	h.codecs[c.id] = c
	h.mu.Unlock()
	go c.run()
	return c.id, nil
}

func (h *wcHost) codec(id int64) *wcCodec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.codecs[id]
}

func (c *wcCodec) submit(j wcJob) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	j.epoch = c.epoch
	c.q = append(c.q, j)
	c.h.pending.Add(1)
	c.cond.Signal()
	c.mu.Unlock()
	return true
}

func (c *wcCodec) discard(j wcJob) {
	if j.hnd != nil {
		c.h.drop(j.hnd.id)
	}
	c.h.pending.Add(-1)
}

func (c *wcCodec) reset() int64 {
	c.mu.Lock()
	c.epoch++
	e := c.epoch
	for _, j := range c.q {
		c.discard(j)
	}
	c.q = nil
	if !c.closed {
		c.q = append(c.q, wcJob{typ: jobReset, epoch: e})
		c.h.pending.Add(1)
		c.cond.Signal()
	}
	c.mu.Unlock()
	return e
}

func (c *wcCodec) close() {
	c.mu.Lock()
	c.closed = true
	for _, j := range c.q {
		c.discard(j)
	}
	c.q = nil
	c.cond.Broadcast()
	c.mu.Unlock()
}

func (c *wcCodec) run() {
	for {
		c.mu.Lock()
		for len(c.q) == 0 && !c.closed {
			c.cond.Wait()
		}
		if c.closed {
			c.mu.Unlock()
			return
		}
		j := c.q[0]
		c.q = c.q[1:]
		cur := c.epoch
		c.mu.Unlock()
		if j.typ != jobReset && j.epoch != cur {
			c.discard(j)
			continue
		}
		c.process(j)
		c.h.pending.Add(-1)
	}
}

func (c *wcCodec) emit(epoch int64, m map[string]any) {
	m["c"], m["e"] = c.id, epoch
	c.h.push(jsonStr(m))
}

func (c *wcCodec) fail(epoch int64, name, f string, a ...any) {
	c.emit(epoch, map[string]any{"t": "err", "n": name, "m": fmt.Sprintf(f, a...)})
}

func (c *wcCodec) process(j wcJob) {
	defer func() {
		if r := recover(); r != nil {
			c.fail(j.epoch, "EncodingError", "codec failure: %v", r)
		}
	}()
	switch j.typ {
	case jobReset:
		if c.vdec != nil {
			c.vdec.Reset()
		}
		if c.adec != nil {
			c.adec.Reset()
		}
		c.fifo, c.fifoSet, c.fifoPos = nil, false, 0
		for _, p := range c.reorder {
			c.h.drop(p.info["h"].(int64))
		}
		c.reorder, c.tsq, c.depth, c.emitted = nil, nil, 0, false
		if c.venc != nil {
			if v, err := media.NewWCVP8Encoder(c.encW, c.encH, c.cfg.Bitrate, c.cfg.Fps, c.cfg.KeyInt); err == nil {
				c.venc = v
			}
			c.sentCfg = false
		}
	case jobDecode:
		c.decode(j)
	case jobEncV:
		c.encodeVideo(j)
	case jobEncA:
		c.encodeAudio(j)
	case jobFlush:
		if c.aenc != nil {
			c.flushAudio(j.epoch)
		}
		if c.vdec != nil && !c.vdec.DecodeOrder() {
			for _, pic := range c.vdec.Flush() {
				c.deliver(j.epoch, pic, wcJob{})
			}
		}
		c.drainFrames(j.epoch, true)
		c.emit(j.epoch, map[string]any{"t": "fl", "f": j.flush})
	}
}

func (c *wcCodec) decode(j wcJob) {
	switch c.kind {
	case "vd":
		pics, err := c.vdec.Decode(j.data)
		if !c.vdec.DecodeOrder() { // HEVC: remember the chunk's timestamp for the picture that will carry it
			c.tsq = append(c.tsq, wcPending{ts: j.ts, dur: j.dur, hasDur: j.hasDur})
			for i := len(c.tsq) - 1; i > 0 && c.tsq[i].ts < c.tsq[i-1].ts; i-- {
				c.tsq[i], c.tsq[i-1] = c.tsq[i-1], c.tsq[i]
			}
		}
		if len(pics) == 0 {
			if err != nil {
				c.fail(j.epoch, "EncodingError", "Decoding error: %v", err)
				return
			}
			c.emit(j.epoch, map[string]any{"t": "deq"})
			return
		}
		for _, pic := range pics {
			if !c.deliver(j.epoch, pic, j) {
				return
			}
		}
	case "ad":
		planar, rate, err := c.adec.Decode(j.data)
		if err != nil {
			c.fail(j.epoch, "EncodingError", "Decoding error: %v", err)
			return
		}
		if len(planar) > 0 && len(planar[0]) > 0 {
			d := wcAudioFromPlanar(planar, rate)
			hd := wcNewHandle(c.h, d, 0, 0, 0, 0)
			if hd == nil {
				c.fail(j.epoch, "QuotaExceededError", "too many open AudioData objects")
				return
			}
			c.emit(j.epoch, map[string]any{"t": "ad", "i": hd.info(), "ts": j.ts})
		}
	}
	c.emit(j.epoch, map[string]any{"t": "deq"})
}

// deliver turns a decoded picture into a frame handle and queues it for output. job carries the timestamps of
// the chunk the picture came from (decode-order codecs); HEVC pictures take theirs from tsq.
func (c *wcCodec) deliver(epoch int64, pic *media.WCPicture, job wcJob) bool {
	d := &wcData{format: pic.Format, w: pic.Width, h: pic.Height, planes: pic.Planes, strides: pic.Strides, matrix: pic.Matrix, full: pic.FullRange}
	for _, p := range d.planes {
		d.bytes += int64(len(p))
	}
	hd := wcNewHandle(c.h, d, 0, 0, pic.Width, pic.Height)
	if hd == nil {
		c.fail(epoch, "QuotaExceededError", "too many open VideoFrames: close() the frames you are done with")
		return false
	}
	ts, dur, has := job.ts, job.dur, job.hasDur
	if !c.vdec.DecodeOrder() {
		if len(c.tsq) > 0 {
			ts, dur, has = c.tsq[0].ts, c.tsq[0].dur, c.tsq[0].hasDur
			c.tsq = c.tsq[1:]
		}
	}
	c.reorder = append(c.reorder, wcPending{info: hd.info(), ts: ts, dur: dur, hasDur: has})
	for i := len(c.reorder) - 1; i > 0 && c.reorder[i].ts < c.reorder[i-1].ts; i-- {
		c.reorder[i], c.reorder[i-1] = c.reorder[i-1], c.reorder[i]
	}
	if c.emitted && ts < c.lastEmit && c.depth < 8 {
		c.depth++
	}
	c.drainFrames(epoch, false)
	return true
}

// drainFrames delivers waiting frames in timestamp order: all of them (flush), or those beyond the reorder depth.
func (c *wcCodec) drainFrames(epoch int64, all bool) {
	depth := c.depth
	if c.vdec != nil {
		depth = max(depth, c.vdec.ReorderDepth())
	}
	for len(c.reorder) > 0 && (all || len(c.reorder) > depth) {
		p := c.reorder[0]
		c.reorder = c.reorder[1:]
		c.lastEmit, c.emitted = p.ts, true
		c.emit(epoch, map[string]any{"t": "vf", "i": p.info, "ts": p.ts, "dur": p.dur, "hd": p.hasDur})
	}
}

func (c *wcCodec) encodeVideo(j wcJob) {
	defer c.h.drop(j.hnd.id)
	d := j.hnd.d
	w, h := j.hnd.vw, j.hnd.vh
	if w < 16 || h < 16 {
		c.fail(j.epoch, "NotSupportedError", "frame %dx%d is smaller than the 16x16 the VP8 encoder takes", w, h)
		return
	}
	var y, u, v []byte
	if (d.format == "I420" || d.format == "I420A") && j.hnd.vx%2 == 0 && j.hnd.vy%2 == 0 {
		y, u, v = d.toI420(j.hnd.vx, j.hnd.vy, w, h)
	} else {
		y, u, v = d.toI420(j.hnd.vx, j.hnd.vy, w, h)
	}
	data, key, err := c.venc.Encode(w, h, y, u, v, w, (w+1)/2, j.key)
	if err != nil {
		c.fail(j.epoch, "EncodingError", "VP8 encode failed: %v", err)
		return
	}
	if w != c.encW || h != c.encH {
		c.encW, c.encH = w, h
		c.sentCfg = false
	}
	if data != nil {
		m := map[string]any{"t": "ec", "b": c.h.putBlob(data), "k": key, "ts": j.ts, "dur": j.dur, "hd": j.hasDur}
		if !c.sentCfg {
			c.sentCfg = true
			m["dc"] = map[string]any{"codec": c.cfg.Codec, "cw": w, "ch": h}
		}
		c.emit(j.epoch, m)
	}
	c.emit(j.epoch, map[string]any{"t": "deq"})
}

func (c *wcCodec) encodeAudio(j wcJob) {
	defer c.h.drop(j.hnd.id)
	d := j.hnd.d
	if d.rate != c.cfg.Rate || d.nch != c.cfg.Ch {
		c.fail(j.epoch, "EncodingError", "AudioData is %d Hz, %d channels; the encoder is configured for %d Hz, %d channels", d.rate, d.nch, c.cfg.Rate, c.cfg.Ch)
		return
	}
	if !c.fifoSet {
		c.fifoSet, c.fifoTS, c.fifoPos = true, j.ts, 0
	}
	for i := 0; i < d.frames; i++ {
		for ch := 0; ch < d.nch; ch++ {
			c.fifo = append(c.fifo, d.pcm[ch][i])
		}
	}
	n := c.aenc.FrameSamples() * c.cfg.Ch
	for len(c.fifo) >= n {
		c.emitOpus(j.epoch, c.fifo[:n])
		c.fifo = c.fifo[n:]
	}
	c.emit(j.epoch, map[string]any{"t": "deq"})
}

func (c *wcCodec) emitOpus(epoch int64, pcm []float32) {
	pkt, err := c.aenc.Encode(pcm)
	if err != nil {
		c.fail(epoch, "EncodingError", "Opus encode failed: %v", err)
		return
	}
	fs := int64(c.aenc.FrameSamples())
	ts := c.fifoTS + c.fifoPos*1000000/int64(c.cfg.Rate)
	dur := fs * 1000000 / int64(c.cfg.Rate)
	c.fifoPos += fs
	m := map[string]any{"t": "ec", "b": c.h.putBlob(pkt), "k": true, "ts": ts, "dur": dur, "hd": true}
	if !c.sentCfg {
		c.sentCfg = true
		m["dc"] = map[string]any{"codec": "opus", "rate": c.cfg.Rate, "ch": c.cfg.Ch, "desc": base64.StdEncoding.EncodeToString(media.OpusHeadBytes(c.cfg.Ch, c.cfg.Rate, 312))}
	}
	c.emit(epoch, m)
}

func (c *wcCodec) flushAudio(epoch int64) {
	n := c.aenc.FrameSamples() * c.cfg.Ch
	if len(c.fifo) > 0 {
		pad := make([]float32, n)
		copy(pad, c.fifo)
		c.fifo = nil
		c.emitOpus(epoch, pad)
	}
	c.fifoSet, c.fifoPos = false, 0
}

// ---- natives and host functions -------------------------------------------------------------------------

func (j *jsVM) wcHost(f map[string]hostFn) {
	h := j.wcState()
	if wasmQJS() == nil && j.vm != nil {
		h.registerNatives()
	}
	f["wcInfo"] = func(a []any) (any, error) {
		hd := wcLookup(argI(a, 0))
		if hd == nil {
			return nil, nil
		}
		return jsonStr(hd.info()), nil
	}
	f["wcRelease"] = func(a []any) (any, error) { h.drop(argI(a, 0)); return nil, nil }
	f["wcClone"] = func(a []any) (any, error) { // (handle) or (handle, x, y, w, h)
		hd := wcLookup(argI(a, 0))
		if hd == nil {
			return nil, hostErrOf(fmt.Errorf("InvalidStateError:the frame is closed"))
		}
		vx, vy, vw, vh := hd.vx, hd.vy, hd.vw, hd.vh
		if len(a) >= 5 {
			vx, vy, vw, vh = int(argI(a, 1)), int(argI(a, 2)), int(argI(a, 3)), int(argI(a, 4))
		}
		n := wcNewHandle(h, hd.d, vx, vy, vw, vh)
		if n == nil {
			return nil, hostErrOf(fmt.Errorf("QuotaExceededError:too many open VideoFrames"))
		}
		return jsonStr(n.info()), nil
	}
	f["wcAlloc"] = func(a []any) (any, error) {
		hd := wcLookup(argI(a, 0))
		if hd == nil || hd.d.audio {
			return nil, hostErrOf(fmt.Errorf("InvalidStateError:the frame is closed"))
		}
		var o wcCopyOpts
		_ = json.Unmarshal([]byte(argS(a, 1)), &o)
		plan, err := planCopy(hd, &o)
		if err != nil {
			return nil, hostErrOf(err)
		}
		return jsonStr(map[string]any{"size": plan.size, "layout": plan.layout}), nil
	}
	f["wcAudioAlloc"] = func(a []any) (any, error) {
		hd := wcLookup(argI(a, 0))
		if hd == nil || !hd.d.audio {
			return nil, hostErrOf(fmt.Errorf("InvalidStateError:the AudioData is closed"))
		}
		var o wcAudioOpts
		_ = json.Unmarshal([]byte(argS(a, 1)), &o)
		o.defaults(hd.d)
		n, err := hd.d.audioSize(o.Format, o.PlaneIndex, o.FrameOffset, o.FrameCount)
		if err != nil {
			return nil, hostErrOf(err)
		}
		return n, nil
	}
	f["wcSupported"] = func(a []any) (any, error) {
		var cfg wcCfg
		if json.Unmarshal([]byte(argS(a, 1)), &cfg) != nil {
			return false, nil
		}
		c, err := wcBuildSupport(argS(a, 0), &cfg)
		if err != nil {
			return false, nil
		}
		_ = c
		return true, nil
	}
	f["wcNew"] = func(a []any) (any, error) {
		var cfg wcCfg
		if err := json.Unmarshal([]byte(argS(a, 1)), &cfg); err != nil {
			return nil, hostErrOf(fmt.Errorf("TypeError:bad configuration"))
		}
		id, err := h.newCodec(argS(a, 0), &cfg)
		if err != nil {
			return nil, hostErrOf(err)
		}
		return id, nil
	}
	f["wcEncodeV"] = func(a []any) (any, error) {
		c := h.codec(argI(a, 0))
		hd := wcLookup(argI(a, 1))
		if c == nil || hd == nil || hd.d.audio {
			return false, nil
		}
		var m struct {
			Ts, Dur int64
			HasDur  bool
			Key     bool
		}
		_ = json.Unmarshal([]byte(argS(a, 2)), &m)
		cl := wcNewHandle(h, hd.d, hd.vx, hd.vy, hd.vw, hd.vh)
		if cl == nil {
			return nil, hostErrOf(fmt.Errorf("QuotaExceededError:too many open VideoFrames"))
		}
		if !c.submit(wcJob{typ: jobEncV, hnd: cl, ts: m.Ts, dur: m.Dur, hasDur: m.HasDur, key: m.Key}) {
			h.drop(cl.id)
			return false, nil
		}
		return true, nil
	}
	f["wcEncodeA"] = func(a []any) (any, error) {
		c := h.codec(argI(a, 0))
		hd := wcLookup(argI(a, 1))
		if c == nil || hd == nil || !hd.d.audio {
			return false, nil
		}
		cl := wcNewHandle(h, hd.d, 0, 0, 0, 0)
		if cl == nil {
			return nil, hostErrOf(fmt.Errorf("QuotaExceededError:too many open AudioData objects"))
		}
		if !c.submit(wcJob{typ: jobEncA, hnd: cl, ts: argI(a, 2)}) {
			h.drop(cl.id)
			return false, nil
		}
		return true, nil
	}
	f["wcFlush"] = func(a []any) (any, error) {
		if c := h.codec(argI(a, 0)); c != nil {
			c.submit(wcJob{typ: jobFlush, flush: argI(a, 1)})
		}
		return nil, nil
	}
	f["wcReset"] = func(a []any) (any, error) {
		if c := h.codec(argI(a, 0)); c != nil {
			return int(c.reset()), nil
		}
		return 0, nil
	}
	f["wcClose"] = func(a []any) (any, error) {
		h.mu.Lock()
		c := h.codecs[argI(a, 0)]
		delete(h.codecs, argI(a, 0))
		h.mu.Unlock()
		if c != nil {
			c.close()
		}
		return nil, nil
	}
	f["wcShare"] = func(a []any) (any, error) { return h.share(argI(a, 0), argB(a, 1)) }
	f["wcAdopt"] = func(a []any) (any, error) { return h.adopt(argS(a, 0)) }
}

func (h *wcHost) registerNatives() {
	vm := h.j.vm
	raw := rawOf(vm)
	reg := func(name string, f any) { _ = vm.RegisterFunc("__swWc_"+name, f, false) }
	bytesOf := func(buf quickjs.Value, off, n any) ([]byte, bool) {
		b, ok := raw.bufferBytes(&buf)
		o, l := int(toInt(off)), int(toInt(n))
		if !ok || o < 0 || l < 0 || o+l > len(b) {
			return nil, false
		}
		return b[o : o+l], true
	}
	reg("fromBuf", func(buf quickjs.Value, off, n, initJSON any) any {
		src, ok := bytesOf(buf, off, n)
		if !ok {
			return wcErr("TypeError", "the data is not a valid buffer")
		}
		var in struct {
			Format         string
			W, H           int
			Layout         []wcLayout
			Matrix         string
			Full           bool
			Prim, Trc      string
			Vx, Vy, Vw, Vh int
		}
		_ = json.Unmarshal([]byte(anyString2(initJSON)), &in)
		var l []wcLayout
		if len(in.Layout) > 0 {
			l = in.Layout
		}
		d, err := wcFromBytes(in.Format, in.W, in.H, src, l)
		if err != nil {
			return splitErr(err)
		}
		d.matrix, d.full, d.prim, d.trc = in.Matrix, in.Full, in.Prim, in.Trc
		if in.Vw == 0 {
			in.Vx, in.Vy, in.Vw, in.Vh = 0, 0, in.W, in.H
		}
		hd := wcNewHandle(h, d, in.Vx, in.Vy, in.Vw, in.Vh)
		if hd == nil {
			return wcErr("QuotaExceededError", "too many open VideoFrames: close() the frames you are done with")
		}
		return jsonStr(hd.info())
	})
	reg("copy", func(hid any, buf quickjs.Value, off, n, optsJSON any) any {
		hd := wcLookup(toInt(hid))
		if hd == nil || hd.d.audio {
			return wcErr("InvalidStateError", "the VideoFrame is closed")
		}
		var o wcCopyOpts
		_ = json.Unmarshal([]byte(anyString2(optsJSON)), &o)
		plan, err := planCopy(hd, &o)
		if err != nil {
			return splitErr(err)
		}
		dst, ok := bytesOf(buf, off, n)
		if !ok || len(dst) < plan.size {
			return wcErr("TypeError", "the destination buffer is too small (%d bytes, %d needed)", len(dst), plan.size)
		}
		if err := plan.run(hd, dst); err != nil {
			return splitErr(err)
		}
		return jsonStr(plan.layout)
	})
	reg("audioNew", func(buf quickjs.Value, off, n, initJSON any) any {
		src, ok := bytesOf(buf, off, n)
		if !ok {
			return wcErr("TypeError", "the data is not a valid buffer")
		}
		var in struct {
			Format       string
			Rate, Ch, Nf int
		}
		_ = json.Unmarshal([]byte(anyString2(initJSON)), &in)
		d, err := wcAudioFromBytes(in.Format, in.Rate, in.Ch, in.Nf, src)
		if err != nil {
			return splitErr(err)
		}
		d.format = in.Format
		hd := wcNewHandle(h, d, 0, 0, 0, 0)
		if hd == nil {
			return wcErr("QuotaExceededError", "too many open AudioData objects")
		}
		return jsonStr(hd.info())
	})
	reg("audioCopy", func(hid any, buf quickjs.Value, off, n, optsJSON any) any {
		hd := wcLookup(toInt(hid))
		if hd == nil || !hd.d.audio {
			return wcErr("InvalidStateError", "the AudioData is closed")
		}
		var o wcAudioOpts
		_ = json.Unmarshal([]byte(anyString2(optsJSON)), &o)
		o.defaults(hd.d)
		size, err := hd.d.audioSize(o.Format, o.PlaneIndex, o.FrameOffset, o.FrameCount)
		if err != nil {
			return splitErr(err)
		}
		dst, ok := bytesOf(buf, off, n)
		if !ok || len(dst) < size {
			return wcErr("RangeError", "the destination buffer is too small (%d bytes, %d needed)", len(dst), size)
		}
		hd.d.audioCopy(o.Format, o.PlaneIndex, o.FrameOffset, o.FrameCount, dst)
		return "{}"
	})
	reg("decode", func(id any, buf quickjs.Value, off, n, metaJSON any) any {
		c := h.codec(toInt(id))
		if c == nil {
			return false
		}
		src, ok := bytesOf(buf, off, n)
		if !ok {
			return false
		}
		var m struct {
			Ts, Dur int64
			HasDur  bool
		}
		_ = json.Unmarshal([]byte(anyString2(metaJSON)), &m)
		return c.submit(wcJob{typ: jobDecode, data: append([]byte(nil), src...), ts: m.Ts, dur: m.Dur, hasDur: m.HasDur})
	})
	reg("take", func(tok any) quickjs.Value {
		h.mu.Lock()
		b, ok := h.blobs[toInt(tok)]
		delete(h.blobs, toInt(tok))
		h.mu.Unlock()
		if !ok {
			return quickjs.UndefinedValue
		}
		return raw.newCopyBuffer(h.j.vm, b)
	})
	reg("imageOpen", func(buf quickjs.Value, off, n, typ any) any {
		src, ok := bytesOf(buf, off, n)
		if !ok {
			return wcErr("TypeError", "the data is not a valid buffer")
		}
		return h.imageOpen(src, anyString2(typ))
	})
}

func anyString2(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---- copyTo plans -------------------------------------------------------------------------------------------

type wcRect struct{ X, Y, W, H int }

type wcCopyOpts struct {
	Rect   *wcRect    `json:"rect"`
	Layout []wcLayout `json:"layout"`
	Format string     `json:"format"`
}

type wcPlan struct {
	rect   wcRect
	layout []wcLayout
	size   int
	format string
	run    func(hd *wcHandle, dst []byte) error
}

func planCopy(hd *wcHandle, o *wcCopyOpts) (*wcPlan, error) {
	d := hd.d
	r := wcRect{hd.vx, hd.vy, hd.vw, hd.vh}
	if o.Rect != nil {
		r = *o.Rect
		if r.W <= 0 || r.H <= 0 || r.X < 0 || r.Y < 0 || r.X+r.W > d.w || r.Y+r.H > d.h {
			return nil, fmt.Errorf("TypeError:The rect is outside the coded area")
		}
	}
	target := d.format
	if o.Format != "" && o.Format != d.format {
		if !wcIsRGB(o.Format) {
			return nil, fmt.Errorf("NotSupportedError:Cannot convert %s to %s (only conversion to RGB formats is defined)", d.format, o.Format)
		}
		target = o.Format
	}
	for _, p := range wcFormatPlanes(d.format) {
		if target == d.format && (r.X%p.xdiv != 0 || r.Y%p.ydiv != 0) {
			return nil, fmt.Errorf("TypeError:The rect is not aligned to the chroma subsampling of %s", d.format)
		}
	}
	tight, total := wcTight(target, r.W, r.H)
	layout, size := tight, total
	if o.Layout != nil {
		var err error
		if size, err = wcCheckLayout(target, r.W, r.H, o.Layout); err != nil {
			return nil, err
		}
		layout = o.Layout
	}
	pl := &wcPlan{rect: r, layout: layout, size: size, format: target}
	pl.run = func(hd *wcHandle, dst []byte) error {
		if target == hd.d.format {
			return hd.d.copyRect(r.X, r.Y, r.W, r.H, dst, layout)
		}
		hd.d.convertRGB(r.X, r.Y, r.W, r.H, target, dst, layout[0])
		return nil
	}
	return pl, nil
}

type wcAudioOpts struct {
	PlaneIndex  int    `json:"planeIndex"`
	FrameOffset int    `json:"frameOffset"`
	FrameCount  int    `json:"frameCount"`
	Format      string `json:"format"`
}

func (o *wcAudioOpts) defaults(d *wcData) {
	if o.Format == "" {
		o.Format = d.format
	}
}

// ---- postMessage of frames --------------------------------------------------------------------------------

var wcTransit sync.Map // token string -> *wcTransitEntry

type wcTransitEntry struct {
	d              *wcData
	vx, vy, vw, vh int
	cl             *agentCluster
}

func (h *wcHost) share(id int64, transfer bool) (any, error) {
	hd := wcLookup(id)
	if hd == nil {
		return nil, hostErrOf(fmt.Errorf("DataCloneError:the frame is closed and cannot be cloned"))
	}
	hd.d.refs.Add(1)
	tok := fmt.Sprintf("%d-%d", wcSeq.Add(1), id)
	wcTransit.Store(tok, &wcTransitEntry{d: hd.d, vx: hd.vx, vy: hd.vy, vw: hd.vw, vh: hd.vh, cl: h.j.agentCluster()})
	if transfer {
		h.drop(id)
	}
	return tok, nil
}

func (h *wcHost) adopt(tok string) (any, error) {
	v, ok := wcTransit.LoadAndDelete(tok)
	if !ok {
		return nil, hostErrOf(fmt.Errorf("DataCloneError:the frame is not available"))
	}
	e := v.(*wcTransitEntry)
	if e.cl != h.j.agentCluster() {
		e.d.release()
		return nil, hostErrOf(fmt.Errorf("DataCloneError:the frame belongs to another agent cluster"))
	}
	hd := wcNewHandle(h, e.d, e.vx, e.vy, e.vw, e.vh)
	e.d.release() // the transit reference
	if hd == nil {
		return nil, hostErrOf(fmt.Errorf("QuotaExceededError:too many open VideoFrames"))
	}
	return jsonStr(hd.info()), nil
}

func init() {
	registerAsyncHandler("wc", func(j *jsVM, e asyncEvent) {
		if _, err := j.call("__swWcEvent", e.data); err != nil && j.tab != nil && !strings.Contains(err.Error(), "not defined") {
			j.tab.LogConsole("error", jsErrorText(err, "WebCodecs callback"))
		}
		j.jobs()
	})
}
