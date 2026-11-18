package claudepty

import (
	"strings"
)

// Input writes bytes to a session's terminal. agentsessions.Manager satisfies it (Service.SendInput routes a
// Caps.PTY session there).
type Input interface {
	SendInput(id string, data []byte) error
}

// Keystrokes the Claude Code TUI reads.
var (
	// keyEnter submits the input box.
	keyEnter = []byte("\r")
	// keyEsc interrupts the running turn. (Twice, on an idle TUI, it opens the rewind menu, so it is only sent while a
	// turn is open.)
	keyEsc = []byte("\x1b")
)

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// pasteBytes is text as one bracketed paste, so newlines stay in the message instead of submitting it. Control
// characters other than newline and tab are removed: an ESC inside the text could end the paste early and turn the
// rest into keystrokes.
func pasteBytes(text string) []byte {
	return []byte(pasteStart + cleanText(text) + pasteEnd)
}

func cleanText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, text)
}

// sameInput reports whether the transcript's prompt text is the text a Submit pasted. The TUI keeps a pasted message
// as it was, so this is an exact comparison up to whitespace.
func sameInput(submitted, seen string) bool {
	return strings.Join(strings.Fields(cleanText(submitted)), " ") == strings.Join(strings.Fields(seen), " ")
}
