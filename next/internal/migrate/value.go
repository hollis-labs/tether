package migrate

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"
)

// kind is a column's storage kind on the target, from its declared type. Values are canonicalized by the
// kind of the column they belong to, so a digest of the source rows and a digest of the target rows agree
// when the rows hold the same values, whichever dialect the target is.
type kind byte

const (
	kindInt  kind = 'I'
	kindReal kind = 'R'
	kindText kind = 'T'
	kindBlob kind = 'B'
)

// kindOf maps a declared type to its kind: the SQLite declared types the schema uses, and the Postgres
// types they map to (migrations/postgres header).
func kindOf(declared string) kind {
	t := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case t == "INTEGER" || t == "BIGINT" || t == "SMALLINT":
		return kindInt
	case t == "REAL" || t == "DOUBLE PRECISION":
		return kindReal
	case t == "BLOB" || t == "BYTEA":
		return kindBlob
	}
	return kindText // TEXT, DATETIME, TIMESTAMP, and the FTS table's untyped columns
}

// canonical renders one value of a column of kind k. NULL is distinct from every value, and every value
// is length-prefixed, so two different rows never render the same. A value whose storage class does not
// fit its column (SQLite's affinity lets a TEXT value sit in an INTEGER column) renders with its own
// class, so it differs from any value the target could hold and the range is reported, not hidden.
func canonical(k kind, v any) []byte {
	var tag byte
	var body string
	switch x := v.(type) {
	case nil:
		return []byte{'N'}
	case int64:
		tag, body = 'I', strconv.FormatInt(x, 10)
		if k == kindReal {
			tag, body = 'R', strconv.FormatFloat(float64(x), 'g', -1, 64)
		}
	case float64:
		tag, body = 'R', strconv.FormatFloat(x, 'g', -1, 64)
	case bool: // Postgres booleans never occur in this schema; keep them distinct if they ever do
		tag, body = 'L', strconv.FormatBool(x)
	case string:
		tag, body = 'T', x
		if k == kindBlob {
			tag = 'B'
		}
	case []byte:
		tag, body = 'B', string(x)
		if k == kindText {
			tag = 'T'
		}
	case time.Time:
		tag, body = 'D', x.UTC().Format(time.RFC3339Nano)
	default:
		tag, body = '?', fmt.Sprintf("%T:%v", v, v)
	}
	out := make([]byte, 0, len(body)+12)
	out = append(out, tag)
	out = binary.AppendUvarint(out, uint64(len(body)))
	return append(out, body...)
}

// rowDigest accumulates the canonical form of ordered rows.
type rowDigest struct {
	h    hash.Hash
	rows int64
}

func newRowDigest() *rowDigest { return &rowDigest{h: sha256.New()} }

func (d *rowDigest) add(kinds []kind, vals []any) {
	var buf []byte
	for i, v := range vals {
		buf = append(buf, canonical(kinds[i], v)...)
	}
	var n [binary.MaxVarintLen64]byte
	d.h.Write(n[:binary.PutUvarint(n[:], uint64(len(buf)))])
	d.h.Write(buf)
	d.rows++
}

func (d *rowDigest) sum() string { return fmt.Sprintf("%x", d.h.Sum(nil)) }
