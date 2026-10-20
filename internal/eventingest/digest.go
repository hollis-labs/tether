package eventingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// digestExcluded are the envelope fields the hub or the transport owns, which
// can differ between two sends of the same event: the hub assigns recorded_at
// and cursor and overwrites actor. Everything else the producer sent is part of
// the event's identity.
var digestExcluded = []string{"recorded_at", "cursor", "actor"}

// eventDigest identifies the event as the producer sent it: the SHA-256 of the
// whole envelope in canonical form (RFC 8785, JCS) minus digestExcluded. The
// same event resent has the same digest (key order and whitespace do not
// matter); any producer-controlled difference under the same id is a
// different event.
func eventDigest(raw []byte) (string, error) {
	if err := strictJSON(raw); err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("digest: %w", err)
	}
	for _, k := range digestExcluded {
		delete(v, k)
	}
	var b bytes.Buffer
	if err := jcs(&b, v); err != nil {
		return "", err
	}
	sum := sha256.Sum256(b.Bytes())
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// jcs writes v, decoded with UseNumber, as RFC 8785 canonical JSON.
func jcs(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return err
		}
		s, err := jcsNumber(f)
		if err != nil {
			return err
		}
		b.WriteString(s)
	case string:
		jcsString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := jcs(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// Members sort by their UTF-16 code units.
		sort.Slice(keys, func(i, j int) bool { return utf16Less(keys[i], keys[j]) })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			jcsString(b, k)
			b.WriteByte(':')
			if err := jcs(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("digest: unexpected %T", v)
	}
	return nil
}

func utf16Less(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

func jcsString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jcsNumber formats f as ECMAScript's Number-to-string, which RFC 8785 requires.
func jcsNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("digest: non-finite number")
	}
	if f == 0 {
		return "0", nil
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// Shortest digits that round-trip, as d.ddde±x.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expS, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expS)
	k, n := len(digits), exp+1 // value = 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k), nil
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:], nil
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits, nil
	}
	ex := n - 1
	es := "+"
	if ex < 0 {
		es, ex = "-", -ex
	}
	if k == 1 {
		return sign + digits + "e" + es + strconv.Itoa(ex), nil
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + es + strconv.Itoa(ex), nil
}

// strictJSON refuses what RFC 8785 (section 3.1 and 3.2.2.2) forbids and what
// encoding/json would silently repair: invalid UTF-8, a lone surrogate escape,
// and a repeated property name at any depth. It runs on the raw bytes, before any
// lossy decode, and keeps U+FFFD written as itself and valid surrogate pairs.
func strictJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("digest: invalid UTF-8")
	}
	if err := checkEscapes(raw); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// keys holds the member names seen so far in each open object; nil marks an array.
	var keys []map[string]bool
	var expectKey []bool
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("digest: %w", err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				keys = append(keys, map[string]bool{})
				expectKey = append(expectKey, true)
				continue
			case '[':
				keys = append(keys, nil)
				expectKey = append(expectKey, false)
				continue
			default: // '}' or ']'
				keys = keys[:len(keys)-1]
				expectKey = expectKey[:len(expectKey)-1]
			}
		case string:
			if n := len(keys); n > 0 && keys[n-1] != nil && expectKey[n-1] {
				if keys[n-1][t] {
					return fmt.Errorf("digest: duplicate property %q", t)
				}
				keys[n-1][t] = true
				expectKey[n-1] = false
				continue
			}
		}
		// A value (or a closed container) was consumed: the next token in an object is a name.
		if n := len(keys); n > 0 && keys[n-1] != nil {
			expectKey[n-1] = true
		}
	}
}

// checkEscapes walks the string literals of raw and rejects a \u escape that is a
// lone surrogate.
func checkEscapes(raw []byte) error {
	in := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !in {
			in = c == '"'
			continue
		}
		switch c {
		case '"':
			in = false
		case '\\':
			i++
			if i >= len(raw) {
				return errors.New("digest: truncated escape")
			}
			if raw[i] != 'u' {
				continue
			}
			r, ok := hex4(raw, i+1)
			if !ok {
				return errors.New("digest: bad unicode escape")
			}
			i += 4
			switch {
			case r >= 0xDC00 && r <= 0xDFFF:
				return errors.New("digest: lone surrogate escape")
			case r >= 0xD800 && r <= 0xDBFF:
				lo, ok := rune(0), false
				if i+6 < len(raw) && raw[i+1] == '\\' && raw[i+2] == 'u' {
					lo, ok = hex4(raw, i+3)
				}
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return errors.New("digest: lone surrogate escape")
				}
				i += 6
			}
		}
	}
	return nil
}

func hex4(b []byte, at int) (rune, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	var r rune
	for _, c := range b[at : at+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}
