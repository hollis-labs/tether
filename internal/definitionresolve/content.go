// Package definitionresolve verifies authored definitions and provides an inert
// mesh resolver. Production composition does not instantiate it yet.
package definitionresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const ContentProtocol = "agentdef-content-v1"
const FileContent = "file"
const TreeContent = "tree"

var ErrContent = errors.New("definition content refused")

type ContentPin struct {
	Kind   string
	Digest string
}

// ContentProvider is a trusted, explicitly configured content port. Implementors
// must enforce confinement and stable read ownership; verification grants no
// authority to use a subsequently reopened mutable resource. Missing authored
// content must return ErrContent, while a missing host root is operational; bare
// ENOENT from a provider fails directory queries rather than omitting a row.
type ContentProvider interface {
	ReadDefinition(context.Context, string) ([]byte, error)
	Pin(context.Context, string) (ContentPin, error)
}

// LocalContent keeps a descriptor for a host-authorized, canonicalized root.
// It never expands environment values or fetches network content. The host must
// keep this authored tree stable during verification and any later use.
type LocalContent struct {
	root      *os.Root
	canonical string
}

func NewLocalContent(authorizedRoot string) (*LocalContent, error) {
	canonical, err := filepath.EvalSymlinks(authorizedRoot)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, err
	}
	return &LocalContent{root: root, canonical: canonical}, nil
}
func (p *LocalContent) Close() error { return p.root.Close() }

func contentError(reason string) error { return fmt.Errorf("%w: %s", ErrContent, reason) }

// Missing paths inside an already opened root are stale authored content.
// A vanished or replaced host root and other I/O faults retain operational errors.
func (p *LocalContent) localContentError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		current, statErr := os.Stat(p.canonical)
		if statErr != nil || !current.IsDir() {
			return err
		}
		opened, statErr := p.root.Stat(".")
		if statErr != nil || !os.SameFile(current, opened) {
			return err
		}
		return contentError("content no longer exists beneath authorized root")
	}
	return err
}
func relative(uri string) (string, error) {
	name, ok := strings.CutPrefix(uri, "catalog:")
	if !ok {
		return "", contentError("unsupported content scheme")
	}
	if err := safeName(name); err != nil {
		return "", err
	}
	return name, nil
}
func safeName(name string) error {
	if name == "" || !utf8.ValidString(name) || !norm.NFC.IsNormalString(name) || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:") {
		return contentError("unsafe or non-NFC relative path")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return contentError("path contains control character")
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return contentError("invalid path component")
		}
	}
	return nil
}
func (p *LocalContent) inspect(name string) (fs.FileInfo, error) {
	var info fs.FileInfo
	parts := strings.Split(name, "/")
	for i := range parts {
		var err error
		info, err = p.root.Lstat(strings.Join(parts[:i+1], "/"))
		if err != nil {
			return nil, p.localContentError(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, contentError("symlink beneath authorized root")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, contentError("non-directory ancestor")
		}
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, contentError("special file")
	}
	return info, nil
}

const maxDefinitionBytes = 4 << 20
const maxContentBytes = 64 << 20
const maxContentFiles = 10000
const maxContentDepth = 64

func (p *LocalContent) readFile(ctx context.Context, name string, limit int64) ([]byte, fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, p.localContentError(err)
	}
	before, err := p.inspect(name)
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, contentError("expected a regular file")
	}
	file, err := openRegular(p.root, name)
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, nil, contentError("file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	if int64(len(data)) > limit {
		return nil, nil, contentError("content size limit exceeded")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	if opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) || opened.Mode() != after.Mode() {
		return nil, nil, contentError("file changed during read")
	}
	current, err := p.inspect(name)
	if err != nil {
		return nil, nil, p.localContentError(err)
	}
	if !os.SameFile(opened, current) {
		return nil, nil, contentError("file replaced during read")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, p.localContentError(err)
	}
	return data, opened, nil
}
func (p *LocalContent) ReadDefinition(ctx context.Context, uri string) ([]byte, error) {
	name, err := relative(uri)
	if err != nil {
		return nil, err
	}
	data, _, err := p.readFile(ctx, name, maxDefinitionBytes)
	return data, err
}

type contentEntry struct {
	name       string
	digest     string
	executable bool
}

func (p *LocalContent) Pin(ctx context.Context, uri string) (ContentPin, error) {
	name, err := relative(uri)
	if err != nil {
		return ContentPin{}, err
	}
	info, err := p.inspect(name)
	if err != nil {
		return ContentPin{}, err
	}
	kind := FileContent
	entries := []contentEntry{}
	remaining := int64(maxContentBytes)
	addFile := func(path, member string) error {
		if len(entries) >= maxContentFiles {
			return contentError("file count limit exceeded")
		}
		data, mode, err := p.readFile(ctx, path, remaining)
		if err != nil {
			return err
		}
		remaining -= int64(len(data))
		hash := sha256.Sum256(data)
		entries = append(entries, contentEntry{name: member, digest: hex.EncodeToString(hash[:]), executable: mode.Mode()&0100 != 0})
		return nil
	}
	if info.Mode().IsRegular() {
		if err := addFile(name, "content"); err != nil {
			return ContentPin{}, err
		}
	} else {
		kind = TreeContent
		seen := map[string]bool{}
		visited := 0
		err = fs.WalkDir(p.root.FS(), name, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return p.localContentError(walkErr)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if path == name {
				return nil
			}
			visited++
			if visited > maxContentFiles*2 {
				return contentError("member count limit exceeded")
			}
			member := strings.TrimPrefix(path, name+"/")
			if err := safeName(member); err != nil {
				return err
			}
			if strings.Count(member, "/") >= maxContentDepth {
				return contentError("tree depth limit exceeded")
			}
			folded := norm.NFC.String(cases.Fold().String(member))
			if seen[folded] {
				return contentError("case-folded member collision")
			}
			seen[folded] = true
			memberInfo, err := p.inspect(path)
			if err != nil {
				return err
			}
			if memberInfo.IsDir() {
				return nil
			}
			return addFile(path, member)
		})
		if err != nil {
			return ContentPin{}, err
		}
	}
	if len(entries) == 0 {
		return ContentPin{}, contentError("content has no regular files")
	}
	// Raw UTF-8 byte order, independent of traversal order or case folding.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%s\n%s\n", ContentProtocol, kind)
	for _, entry := range entries {
		executable := 0
		if entry.executable {
			executable = 1
		}
		_, _ = fmt.Fprintf(hash, "%s\t%d\t%s\n", entry.digest, executable, entry.name)
	}
	// Recheck selected ancestors so replacement by a link is refused as well.
	current, err := p.inspect(name)
	if err != nil {
		return ContentPin{}, err
	}
	if !os.SameFile(info, current) {
		return ContentPin{}, contentError("selected root changed")
	}
	return ContentPin{Kind: kind, Digest: "sha256:" + hex.EncodeToString(hash.Sum(nil))}, nil
}
