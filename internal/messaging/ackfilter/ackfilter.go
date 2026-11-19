// Package ackfilter detects pure acknowledgement replies ("Noted. No reply
// needed.", "Thanks", "ack") so the hub can refuse them at send time
// (CW-20261118-0050): each one wakes its recipient for a turn that has nothing
// to do. The detector is deliberately conservative: the whole body must be made
// of acknowledgement words, so any real content passes.
package ackfilter

import (
	"strings"
	"unicode"
)

// maxWords bounds an acknowledgement; longer bodies always pass.
const maxWords = 10

// phrases are the acknowledgement vocabulary, as lower-case word sequences. A
// body is an acknowledgement only when it is entirely a sequence of these.
var phrases = [][]string{
	{"noted"}, {"ack"}, {"acked"}, {"acknowledged"},
	{"thanks"}, {"thx"}, {"ty"}, {"thank", "you"}, {"many", "thanks"}, {"much", "appreciated"}, {"appreciated"},
	{"no", "reply", "needed"}, {"no", "reply", "necessary"}, {"no", "reply", "required"},
	{"no", "response", "needed"}, {"no", "response", "required"},
	{"no", "action", "needed"}, {"no", "action", "required"}, {"no", "action", "necessary"},
	{"and"}, {"for", "the", "update"}, {"for", "the", "info"}, {"the", "update"},
}

// IsAck reports whether body is nothing but an acknowledgement. A question mark
// or any word outside the vocabulary makes it real content.
func IsAck(body string) bool {
	if strings.ContainsRune(body, '?') {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(body), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	if len(words) == 0 || len(words) > maxWords {
		return false
	}
	// Must contain at least one anchor word, so "and" or "fine" alone is not enough.
	return covers(words) && anchored(words)
}

func covers(words []string) bool {
	if len(words) == 0 {
		return true
	}
	for _, p := range phrases {
		if len(p) <= len(words) && equal(words[:len(p)], p) && covers(words[len(p):]) {
			return true
		}
	}
	return false
}

var anchors = map[string]bool{"noted": true, "ack": true, "acked": true, "acknowledged": true,
	"thanks": true, "thx": true, "ty": true, "thank": true, "no": true, "appreciated": true}

func anchored(words []string) bool {
	for _, w := range words {
		if anchors[w] {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Refusal is the message a sender gets back when its acknowledgement is refused.
const Refusal = "this reply is a pure acknowledgement (no answer, status, evidence or question), and it would wake the recipient for a turn with nothing to do. Send nothing: silence means received. If you do have something to add, say it in the reply."
