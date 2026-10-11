package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// flipRecord is one line of run/identity-flips.jsonl, the hash-chained audit of
// identity mode changes (CW-20261010-0268). Prev is the previous record's Hash
// (genesisHash for the first); Hash covers every other field. By is an
// unauthenticated label: the verbs run as the operator's uid and cannot prove
// who typed them.
type flipRecord struct {
	At       string `json:"at"`
	Action   string `json:"action"` // flip | rollback | reset
	By       string `json:"by"`
	Point    string `json:"point"` // a config.IdentityPoints name, or "global"
	From     string `json:"from"`  // effective mode before the change
	To       string `json:"to"`    // effective mode after the change
	Prior    string `json:"prior"` // raw configured value before the change; "" = key absent
	Evidence string `json:"evidence,omitempty"`
	Catalog  string `json:"catalog"`
	Prev     string `json:"prev"`
	Hash     string `json:"hash"`
}

const genesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

func (r flipRecord) computeHash() string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// verifyFlipLog reads the whole log and checks the chain. Every line must parse
// and link; the first violation is returned as an error that names the line.
// A missing file is an empty, valid log. The head hash is returned so the owner
// can pin it in the GO record: truncation of the tail is detectable only against
// a head hash recorded elsewhere.
func verifyFlipLog(path string) (recs []flipRecord, head string, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: run dir beside the catalog
	if os.IsNotExist(err) {
		return nil, genesisHash, nil
	}
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }()
	head = genesisHash
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			return recs, head, fmt.Errorf("identity flip log %s line %d: blank line", path, n)
		}
		var r flipRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return recs, head, fmt.Errorf("identity flip log %s line %d: malformed record", path, n)
		}
		if r.Prev != head {
			return recs, head, fmt.Errorf("identity flip log %s line %d: chain broken (prev does not match preceding record; record removed, reordered or inserted)", path, n)
		}
		if r.Hash != r.computeHash() {
			return recs, head, fmt.Errorf("identity flip log %s line %d: record altered (hash mismatch)", path, n)
		}
		recs = append(recs, r)
		head = r.Hash
	}
	if err := sc.Err(); err != nil {
		return recs, head, err
	}
	return recs, head, nil
}

// appendFlip chains r onto the log under an exclusive lock. It refuses to
// append to a log that does not verify.
func appendFlip(path string, r *flipRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: run dir beside the catalog
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // G115: fd fits int
		return err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }() //nolint:gosec // G115
	_, head, err := verifyFlipLog(path)
	if err != nil {
		return fmt.Errorf("refusing to append: %w", err)
	}
	r.Prev = head
	r.Hash = r.computeHash()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: run dir beside the catalog
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// pendingFlips replays the log: for each target, the stack of flips not yet
// rolled back (a rollback pops the newest; a reset clears the target).
func pendingFlips(recs []flipRecord) map[string][]flipRecord {
	out := map[string][]flipRecord{}
	for _, r := range recs {
		switch r.Action {
		case "flip":
			out[r.Point] = append(out[r.Point], r)
		case "rollback":
			if s := out[r.Point]; len(s) > 0 {
				out[r.Point] = s[:len(s)-1]
			}
		case "reset":
			delete(out, r.Point)
		}
	}
	return out
}
