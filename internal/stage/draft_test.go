package stage

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// draftLog collects frames and applies them as a client does: offset 0
// replaces, an offset equal to what it holds appends, anything else fails.
type draftLog struct {
	t      *testing.T
	frames []DraftFrame
	held   []rune
	// seen is every text the client held, frame by frame.
	seen []string
}

func newDraftLog(t *testing.T) *draftLog { return &draftLog{t: t} }

func (d *draftLog) add(f DraftFrame) {
	d.t.Helper()

	if f.Void {
		d.held = nil
		d.frames = append(d.frames, f)
		d.seen = append(d.seen, "")

		return
	}

	require.True(d.t, utf8.ValidString(f.Text), "a frame's text is valid UTF-8")
	require.True(d.t, f.Offset == 0 || f.Offset == len(d.held), "frame at offset %d while %d are held", f.Offset, len(d.held))

	if f.Offset == 0 {
		d.held = nil
	}

	d.held = append(d.held, []rune(f.Text)...)
	d.frames = append(d.frames, f)
	d.seen = append(d.seen, string(d.held))
}

func (d *draftLog) text() string { return string(d.held) }

// fakeClock is a clock a test moves by hand.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// feed sends args to x as submit_artifact call c1 at index 0, n bytes at a
// time.
func feed(x *draftExtractor, args string, n int) {
	for i := 0; i < len(args); i += n {
		x.fragment(0, "c1", submitArtifactName, args[i:min(i+n, len(args))])
	}
}

func bodyOf(t *testing.T, args string) string {
	t.Helper()

	var v struct {
		Body string `json:"body"`
	}

	require.NoError(t, json.Unmarshal([]byte(args), &v))

	return v.Body
}

func TestDraft_StreamsTheBodyAcrossAnySplit(t *testing.T) {
	// Data before the body, a nested "body" key that is not the body,
	// escapes, \u sequences, a surrogate pair and multi-byte runes.
	args := `{"data":{"body":"not this","list":["a\"b",{"x":1}]},"body":"# Need\n\nTab\there \"quoted\" back\\slash \u00e9t\u00e9 \ud83d\ude00 ñ 日本 😀 end"}`
	want := bodyOf(t, args)

	for _, n := range []int{1, 2, 3, 5, 7, 64} {
		t.Run(fmt.Sprintf("fragments of %d", n), func(t *testing.T) {
			got := newDraftLog(t)
			x := newDraftExtractor(nil, got.add, time.Now, 0)

			feed(x, args, n)
			x.final(want)

			assert.Equal(t, want, got.text())
			assert.Greater(t, len(got.frames), 1, "the body is published as it grows")
		})
	}
}

func TestDraft_OnlySubmitArtifactIsDrafted(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", "ask_human", `{"body":"a question"}`)
	x.fragment(1, "c2", "read", `{"path":"x","body":"y"}`)

	assert.Empty(t, got.frames)
}

func TestDraft_FragmentsBeforeTheNameAreKept(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", "", `{"bo`)
	x.fragment(0, "c1", "", `dy":"He`)
	x.fragment(0, "c1", submitArtifactName, `llo"}`)

	assert.Equal(t, "Hello", got.text())
}

func TestDraft_AResetRestartsAtOffsetZero(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	feed(x, `{"body":"first try`, 4)
	x.reset()
	feed(x, `{"body":"second"}`, 4)

	assert.Equal(t, "second", got.text())
	assert.Contains(t, got.seen, "first try", "the replayed prefix was shown before the reset")
}

func TestDraft_ANewCallAtTheSameIndexRestarts(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", submitArtifactName, `{"body":"old"}`)
	x.fragment(0, "c2", submitArtifactName, `{"body":"new"}`)

	assert.Equal(t, "new", got.text())
}

func TestDraft_FramesAreCoalesced(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, clock.Now, 250*time.Millisecond)

	x.fragment(0, "c1", submitArtifactName, `{"body":"a`)
	x.fragment(0, "c1", submitArtifactName, `b`)
	x.fragment(0, "c1", submitArtifactName, `c`)
	require.Len(t, got.frames, 1, "one frame inside the interval")

	clock.now = clock.now.Add(300 * time.Millisecond)

	x.fragment(0, "c1", submitArtifactName, `d`)
	require.Len(t, got.frames, 2, "the next fragment after the interval publishes")
	assert.Equal(t, "abcd", got.text())

	x.fragment(0, "c1", submitArtifactName, `e"}`)
	x.final("abcde")
	assert.Equal(t, "abcde", got.text(), "the submit publishes the rest at once")
}

func TestDraft_ASecretSplitAcrossFragmentsIsNeverPublished(t *testing.T) {
	const secret = "sk-secret-0123456789"

	got := newDraftLog(t)
	x := newDraftExtractor([]string{secret}, got.add, time.Now, 0)

	feed(x, `{"body":"key `+secret+` end"}`, 1)
	x.final("key " + secret + " end")

	for _, held := range got.seen {
		assert.NotContains(t, held, "sk-", "no prefix of the secret is ever shown: %q", held)
	}

	assert.Equal(t, "key [REDACTED] end", got.text())
}

func TestDraft_AnEscapedSecretIsMasked(t *testing.T) {
	const secret = "sk-secret-0123456789"

	got := newDraftLog(t)
	x := newDraftExtractor([]string{secret}, got.add, time.Now, 0)

	// The model escaped the hyphens; decoding comes before masking.
	feed(x, `{"body":"key sk\u002dsecret\u002d0123456789 end"}`, 3)

	assert.Equal(t, "key [REDACTED] end", got.text())

	for _, held := range got.seen {
		assert.NotContains(t, held, "secret-01")
	}
}

func TestDraft_ASecretTheCutWouldSplitIsHeldBackWhole(t *testing.T) {
	// The tail starts the second secret while the first is complete across
	// the cut; neither part of the first may show.
	got := newDraftLog(t)
	x := newDraftExtractor([]string{"abcdefgh", "cdefghij"}, got.add, time.Now, 0)

	feed(x, `{"body":"..abcdefghij"}`, 1)

	for _, held := range got.seen {
		assert.NotContains(t, held, "ab", "held %q", held)
	}

	assert.Equal(t, "..[REDACTED]ij", got.text())
}

func TestDraft_ARefusalIsMaskedAndTheNextAttemptRestarts(t *testing.T) {
	const secret = "sk-secret-0123456789"

	got := newDraftLog(t)
	x := newDraftExtractor([]string{secret}, got.add, time.Now, 0)

	feed(x, `{"body":"draft one"}`, 4)
	x.final("draft one")
	x.refuse("1 problem: the body quotes " + secret)

	last := got.frames[len(got.frames)-1]
	assert.True(t, last.Refused)
	assert.Empty(t, last.Text)
	assert.Equal(t, "draft one", got.text(), "the refused draft stays shown")
	assert.Contains(t, last.Reason, "[REDACTED]")
	assert.NotContains(t, last.Reason, secret)

	x.fragment(0, "c9", submitArtifactName, `{"body":"draft two"}`)
	assert.Equal(t, 0, got.frames[len(got.frames)-1].Offset, "the next attempt restarts at offset 0")
	assert.Equal(t, "draft two", got.text())
}

func TestDraft_AFinalBodyThatDoesNotExtendWhatWasSentRestarts(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	feed(x, `{"body":"abc`, 2)
	x.final("xyz")

	assert.Equal(t, 0, got.frames[len(got.frames)-1].Offset)
	assert.Equal(t, "xyz", got.text())
}

func TestDraft_ASubmitWithoutFragmentsIsPublishedWhole(t *testing.T) {
	// A model whose stream carries no tool-call fragments shows its body at
	// submit.
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.final("# Whole")

	require.Len(t, got.frames, 1)
	assert.Equal(t, DraftFrame{Offset: 0, Text: "# Whole"}, got.frames[0])
}

func TestDraft_ABodyThatOpensIsShownAtOnce(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", submitArtifactName, `{"body":"`)

	require.Len(t, got.frames, 1, "an empty frame opens the draft")
	assert.Equal(t, DraftFrame{}, got.frames[0])
}

func TestDraft_AnAbandonedTurnVoidsTheDraft(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.void()
	assert.Empty(t, got.frames, "nothing shown, nothing to void")

	feed(x, `{"body":"half a dra`, 4)
	x.void()

	last := got.frames[len(got.frames)-1]
	assert.True(t, last.Void)
	assert.Empty(t, got.text())

	x.fragment(0, "c1", submitArtifactName, `{"body":"again"}`)
	assert.Equal(t, 0, got.frames[len(got.frames)-1].Offset, "the next attempt starts at offset 0")
	assert.Equal(t, "again", got.text())
}

func TestDraft_ARefusalWithNothingShownPublishesNothing(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.refuse("repaired before the body started")

	assert.Empty(t, got.frames)

	feed(x, `{"body":"draft"}`, 4)
	assert.Equal(t, "draft", got.text())
}

func TestDraft_ARepairOfAnotherCallLeavesTheDraftAlone(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", submitArtifactName, `{"body":"one"}`)

	n := len(got.frames)

	x.refuseCall("c2", "another call's repair")
	assert.Len(t, got.frames, n)

	x.refuseCall("c1", "its own repair")
	require.Len(t, got.frames, n+1)
	assert.True(t, got.frames[n].Refused)
}

func TestDraft_AVoidKeepsARefusedDraft(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	feed(x, `{"body":"draft"}`, 4)
	x.final("draft")
	x.refuse("no")

	n := len(got.frames)

	x.void()

	assert.Len(t, got.frames, n, "the refused draft is the last attempt's and stays")
}

func TestDraft_ANilExtractorDoesNothing(t *testing.T) {
	var x *draftExtractor

	assert.NotPanics(t, func() {
		x.fragment(0, "c1", submitArtifactName, `{"body":"x"}`)
		x.reset()
		x.final("x")
		x.refuse("x")
		x.void()
	})
}

func TestWhole_DropsARuneItsFragmentCutShort(t *testing.T) {
	b := []byte("ab日")
	assert.Equal(t, "ab", whole(b[:len(b)-1]))
	assert.Equal(t, "ab日", whole(b))
	assert.Equal(t, "ab", whole(b[:3]))
}

func TestDraft_AVoidAfterAResetStillVoidsWhatIsShown(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	feed(x, `{"body":"half a dra`, 4)
	x.reset()
	x.void()

	assert.True(t, got.frames[len(got.frames)-1].Void)
	assert.Empty(t, got.text())
}

func TestDraft_AVoidAfterACallSwitchStillVoidsWhatIsShown(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "c1", submitArtifactName, `{"body":"half a dra`)
	x.fragment(0, "c2", submitArtifactName, `{"da`)
	x.void()

	assert.True(t, got.frames[len(got.frames)-1].Void)
	assert.Empty(t, got.text())
}

func TestDraft_ADifferentNameAtTheSameIndexIsANewCall(t *testing.T) {
	got := newDraftLog(t)
	x := newDraftExtractor(nil, got.add, time.Now, 0)

	x.fragment(0, "", "read", `{"path":"x"}`)
	x.fragment(0, "", submitArtifactName, `{"body":"x"}`)

	assert.Equal(t, "x", got.text())
}
