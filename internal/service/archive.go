package service

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const maxArchiveBytes int64 = 512 << 20
const maxExtractedBytes int64 = 2 << 30

var sha256Hex = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

func releaseChecksum(file, archive string) (string, error) {
	data, err := readRegular(file, 1<<20)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	var digest string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != archive {
			continue
		}
		if digest != "" || !sha256Hex.MatchString(fields[0]) {
			return "", fmt.Errorf("ambiguous or invalid checksum for %s", archive)
		}
		digest = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if digest == "" {
		return "", fmt.Errorf("checksums.txt has no SHA-256 entry for %s", archive)
	}
	return digest, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func copyArchive(ctx context.Context, source, destination string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("archive must be a regular file")
	}
	input, err := os.Open(source) //nolint:gosec // explicit local archive, checked regular before open
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // exclusive file in owned staging
	if err != nil {
		return "", err
	}
	defer output.Close()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, digest), io.LimitReader(contextReader{ctx, input}, maxArchiveBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("archive exceeds 512 MiB")
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // owned runtime executable, regular type and byte bound checked
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxArchiveBytes {
		return "", fmt.Errorf("invalid executable file")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, maxArchiveBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxArchiveBytes {
		return "", fmt.Errorf("executable exceeds size limit")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func extractArchive(ctx context.Context, archive, destination string) error {
	file, err := os.Open(archive) //nolint:gosec // owned staging archive already checksum verified
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(contextReader{ctx, gz})
	seen := make(map[string]bool)
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if err == io.EOF {
			// Consume the gzip trailer too: a tar end marker alone cannot
			// certify a complete gzip member. Bound all discarded padding.
			left := maxExtractedBytes - total
			n, err := io.Copy(io.Discard, io.LimitReader(contextReader{ctx, gz}, left+1))
			if err != nil {
				return err
			}
			if n > left {
				return fmt.Errorf("archive exceeds extraction limit")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if count >= 2048 {
			return fmt.Errorf("archive has too many members")
		}
		name := header.Name
		clean := path.Clean(name)
		if strings.Contains(name, "\\") || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("archive path escapes staging: %q", name)
		}
		if clean == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if clean == "." || seen[clean] || strings.HasPrefix(clean, ".install") || strings.HasPrefix(clean, ".archive") {
			return fmt.Errorf("duplicate or reserved archive path: %q", name)
		}
		seen[clean] = true
		full := filepath.Join(destination, filepath.FromSlash(clean))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0700); err != nil {
				return err
			}
		case tar.TypeReg, 0:
			if header.Size < 0 || header.Size > maxExtractedBytes-total {
				return fmt.Errorf("archive exceeds extraction limit")
			}
			total += header.Size
			if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
				return err
			}
			mode := os.FileMode(0600)
			if header.Mode&0111 != 0 {
				mode = 0700
			}
			out, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // contained path; links refused and duplicates exclusive
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(out, reader, header.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive links and special types are refused: %q", name)
		}
	}
}
