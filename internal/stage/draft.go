package stage

import (
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
)

// submitArtifactName is the terminal tool whose body a draft follows.
const submitArtifactName = "submit_artifact"

// draftInterval spaces draft frames while a body streams: about four a
// second.
const draftInterval = 250 * time.Millisecond

// minSecretLen mirrors the harness redactor, which ignores shorter secrets.
const minSecretLen = 6

// DraftFrame is one update of the artifact body a run is writing: Text
// follows the first Offset code points already published, and offset 0
// replaces whatever was shown. Refused marks the body a check refused,
// Reason saying why. Void says the draft so far is void: the harness
// abandoned the turn that wrote it, so no draft is in progress.
type DraftFrame struct {
	Offset  int
	Text    string
	Refused bool
	Reason  string
	Void    bool
}

// draftExtractor follows the body of submit_artifact as its arguments
// stream, and publishes it redacted with the session's secrets as
// accumulated text, never fragment by fragment. While the body grows, a
// tail that could be the start of a secret is held back. Frames go out at
// most every interval; the submit itself publishes the whole body.
type draftExtractor struct {
	publish  func(DraftFrame)
	red      *redact.Redactor
	secrets  []string
	now      func() time.Time
	interval time.Duration

	mu sync.Mutex
	// calls are the streamed tool calls of the current response, by index.
	calls map[int]*draftCall
	// active is the index of the submit call whose body is drafted; -1 none.
	active int
	// opened is set once a frame went out for the active body; sent is the
	// redacted text published for it.
	opened bool
	// shown is set while the client holds a draft frame this extractor sent
	// and no void or refusal has closed it; a reset does not clear it.
	shown bool
	sent  string
	last  time.Time
}

// draftCall is one streamed tool call.
type draftCall struct {
	id, name string
	// early keeps fragments that came before the call's name.
	early []byte
	scan  bodyScanner
}

func newDraftExtractor(secrets []string, publish func(DraftFrame), now func() time.Time, interval time.Duration) *draftExtractor {
	return &draftExtractor{
		publish: publish, red: redact.New(secrets), secrets: secrets, now: now, interval: interval,
		calls: map[int]*draftCall{}, active: -1,
	}
}

// fragment takes one piece of a streamed tool call's arguments. A call is
// known by its index in the response, and a new id at an index is a new
// call. Only submit_artifact's body is drafted; fragments that arrive before
// the call's name are kept until it is known.
func (x *draftExtractor) fragment(index int, id, name, args string) {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	call := x.calls[index]
	if call == nil || (id != "" && call.id != "" && id != call.id) ||
		(name != "" && call.name != "" && name != call.name) {
		call = &draftCall{}
		x.calls[index] = call

		if x.active == index {
			x.active = -1
		}
	}

	if call.id == "" {
		call.id = id
	}

	if call.name == "" {
		call.name = name
	}

	switch {
	case call.name == "":
		call.early = append(call.early, args...)

		return
	case call.name != submitArtifactName:
		call.early = nil

		return
	}

	if x.active != index {
		x.active = index
		x.opened, x.sent, x.last = false, "", time.Time{}
	}

	if len(call.early) > 0 {
		call.scan.feed(string(call.early))
		call.early = nil
	}

	call.scan.feed(args)

	if !call.scan.started {
		return
	}

	if x.now().Sub(x.last) < x.interval {
		return
	}

	x.emit(x.streamed(call))
}

// delta takes one streamed fragment from the harness: a tool call's
// argument piece, or the reset that comes before a replay after a
// mid-stream retry. Content and reasoning are not drafted.
func (x *draftExtractor) delta(d llm.Delta) {
	if d.Reset {
		x.reset()
	}

	if tc := d.ToolCall; tc != nil {
		x.fragment(tc.Index, tc.ID, tc.Name, tc.ArgsFragment)
	}
}

// newModelCall drops the buffered calls of the previous response, so the
// next response never continues a draft by reusing an id. What is shown
// stays until the next frame replaces it.
func (x *draftExtractor) newModelCall() {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	x.clear()
}

// reset forgets everything streamed so far: the harness replays the
// response after a mid-stream retry. The next frame starts at offset 0.
func (x *draftExtractor) reset() {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	x.clear()
}

// final publishes the whole body the submit carried, which is the
// authority on what the model wrote.
func (x *draftExtractor) final(body string) {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	x.emit(body)
}

// refuse marks the published body refused by a check and starts over: the
// next attempt's first frame is at offset 0. With no draft shown there is
// nothing to mark.
func (x *draftExtractor) refuse(reason string) {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	x.refuseShown(reason)
}

// refuseCall is refuse for a call the harness repaired: it applies only
// when id is the call the draft follows, since another submit in the same
// response has a draft of its own.
func (x *draftExtractor) refuseCall(id, reason string) {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	if call := x.calls[x.active]; call == nil || call.id != id {
		return
	}

	x.refuseShown(reason)
}

func (x *draftExtractor) refuseShown(reason string) {
	if x.shown {
		x.publish(DraftFrame{Offset: utf8.RuneCountInString(x.sent), Refused: true, Reason: x.red.Apply(reason)})
		x.shown = false
	}

	x.clear()
}

// void drops the draft of a turn the harness abandoned, which sends no
// reset: a void frame goes out when a draft was shown, and the next attempt
// starts at offset 0. A refused draft is the last attempt's and stays.
func (x *draftExtractor) void() {
	if x == nil {
		return
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	if x.shown {
		x.publish(DraftFrame{Void: true})
		x.shown = false
	}

	x.clear()
}

func (x *draftExtractor) clear() {
	x.calls = map[int]*draftCall{}
	x.active = -1
	x.opened, x.sent, x.last = false, "", time.Time{}
}

// streamed is the part of call's body that may be published now: whole
// runes only and, while the body string is still open, less a tail that
// may start a secret.
func (x *draftExtractor) streamed(call *draftCall) string {
	text := whole(call.scan.body)
	if !call.scan.closed {
		text = text[:cutAt(text, x.secrets)]
	}

	return text
}

// emit publishes what text adds to what was sent, redacted. A text that no
// longer extends what was sent, as when redaction masked part of it, goes
// out whole at offset 0.
func (x *draftExtractor) emit(text string) {
	red := x.red.Apply(strings.ToValidUTF8(text, "�"))
	frame := DraftFrame{Text: red}

	if x.opened && strings.HasPrefix(red, x.sent) {
		frame.Offset = utf8.RuneCountInString(x.sent)
		frame.Text = red[len(x.sent):]

		if frame.Text == "" {
			x.last = x.now()

			return
		}
	}

	x.opened, x.shown, x.sent, x.last = true, true, red, x.now()
	x.publish(frame)
}

// whole is b without a trailing rune its fragment cut short.
func whole(b []byte) string {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return string(b[:i])
			}

			break
		}
	}

	return string(b)
}

// cutAt is how much of a growing text may be published: all but the
// longest tail that is the start of a secret, and never part of a secret
// occurrence the cut would split.
func cutAt(text string, secrets []string) int {
	cut := len(text)

	for _, s := range secrets {
		if len(s) < minSecretLen {
			continue
		}

		for k := min(len(s)-1, len(text)); k > 0 && len(text)-k < cut; k-- {
			if strings.HasSuffix(text, s[:k]) {
				cut = len(text) - k

				break
			}
		}
	}

	for moved := true; moved; {
		moved = false

		for _, s := range secrets {
			if len(s) < minSecretLen {
				continue
			}

			from := max(0, cut-len(s)+1)
			if i := strings.Index(text[from:], s); i >= 0 && from+i < cut {
				cut, moved = from+i, true
			}
		}
	}

	return cut
}

// bodyScanner reads a JSON object streamed in fragments and decodes its
// top-level "body" string as it grows. It keeps only what it needs: the
// nesting, whether it is inside a string, a pending escape, and the
// top-level key it read last.
type bodyScanner struct {
	depth    int
	inString bool
	escaped  bool
	// hex holds the digits of a \u escape read so far; nil outside one.
	hex []byte
	// high is a \u high surrogate waiting for its low half.
	high rune
	// inKey marks a top-level key string; expect a top-level value next.
	inKey   bool
	key     []byte
	lastKey string
	expect  bool
	inBody  bool
	started bool
	closed  bool
	body    []byte
}

func (s *bodyScanner) feed(p string) {
	for i := range len(p) {
		s.step(p[i])
	}
}

func (s *bodyScanner) step(b byte) {
	if s.inString {
		s.stringByte(b)

		return
	}

	switch b {
	case '{', '[':
		s.expect = false
		s.depth++
	case '}', ']':
		s.depth--
	case '"':
		s.inString = true

		switch {
		case s.depth == 1 && s.expect:
			s.expect = false

			if s.lastKey == "body" {
				s.inBody, s.started, s.closed = true, true, false
				s.body = s.body[:0]
			}
		case s.depth == 1:
			s.inKey = true
			s.key = s.key[:0]
		}
	case ':':
		if s.depth == 1 {
			s.expect = true
		}
	case ' ', '\t', '\n', '\r', ',':
	default:
		// A number, true, false or null: a scalar value.
		s.expect = false
	}
}

func (s *bodyScanner) stringByte(b byte) {
	switch {
	case s.hex != nil:
		s.hex = append(s.hex, b)
		if len(s.hex) < 4 {
			return
		}

		n, err := strconv.ParseUint(string(s.hex), 16, 16)
		s.hex = nil

		if err != nil {
			s.char(utf8.RuneError)

			return
		}

		s.unit(rune(n))
	case s.escaped:
		s.escaped = false

		switch b {
		case 'u':
			s.hex = make([]byte, 0, 4)
		case 'b':
			s.char('\b')
		case 'f':
			s.char('\f')
		case 'n':
			s.char('\n')
		case 'r':
			s.char('\r')
		case 't':
			s.char('\t')
		default:
			// \" \\ \/ and anything a lenient model sends.
			s.char(rune(b))
		}
	case b == '\\':
		s.escaped = true
	case b == '"':
		s.flushHigh()
		s.inString = false

		if s.inKey {
			s.inKey = false
			s.lastKey = string(s.key)
		}

		if s.inBody {
			s.inBody, s.closed = false, true
		}
	default:
		s.flushHigh()
		s.raw(b)
	}
}

// unit takes one \u code unit, pairing surrogates as encoding/json does:
// an unpaired half is U+FFFD.
func (s *bodyScanner) unit(r rune) {
	switch {
	case r >= 0xD800 && r < 0xDC00:
		s.flushHigh()
		s.high = r
	case r >= 0xDC00 && r < 0xE000:
		if s.high == 0 {
			s.write(utf8.RuneError)

			return
		}

		pair := utf16.DecodeRune(s.high, r)
		s.high = 0
		s.write(pair)
	default:
		s.char(r)
	}
}

// char writes a decoded rune after any pending high surrogate.
func (s *bodyScanner) char(r rune) {
	s.flushHigh()
	s.write(r)
}

func (s *bodyScanner) flushHigh() {
	if s.high != 0 {
		s.high = 0
		s.write(utf8.RuneError)
	}
}

// write appends r to the string being read when it is one the scanner keeps.
func (s *bodyScanner) write(r rune) {
	switch {
	case s.inKey:
		s.key = utf8.AppendRune(s.key, r)
	case s.inBody:
		s.body = utf8.AppendRune(s.body, r)
	}
}

// raw appends one unescaped byte of the string being read.
func (s *bodyScanner) raw(b byte) {
	switch {
	case s.inKey:
		s.key = append(s.key, b)
	case s.inBody:
		s.body = append(s.body, b)
	}
}
