package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var bootIdentity = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var nonceIdentity = regexp.MustCompile(`^[a-f0-9]{32}$`)

type lockOwner struct {
	PID   int    `json:"pid"`
	Boot  string `json:"boot"`
	Start string `json:"start"`
	Token string `json:"token"`
}

func randomToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

// mkdir remains the durable lock. A short kernel guard serializes creation,
// reclamation and release, avoiding removal of a new owner's replacement lock.
func acquireLock(ctx context.Context, path string) (func(), error) {
	owner, err := processIdentity(os.Getpid())
	if err != nil {
		return nil, err
	}
	owner.Token, err = randomToken()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("install-lock-busy: %w", err)
		}
		guard, err := lockGuard(ctx, path+".guard")
		if err != nil {
			return nil, err
		}
		acquired, err := claimLock(path, owner)
		var ownedInfo os.FileInfo
		if acquired && err == nil {
			ownedInfo, err = os.Lstat(path)
		}
		guard()
		if err != nil {
			return nil, err
		}
		if acquired {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				guard, err := lockGuard(releaseCtx, path+".guard")
				if err != nil {
					return
				}
				defer guard()
				current, err := readLockOwner(path)
				currentInfo, infoErr := os.Lstat(path)
				if err == nil && infoErr == nil && os.SameFile(ownedInfo, currentInfo) && current == owner {
					_ = os.RemoveAll(path)
				}
			}, nil
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func readLockOwner(path string) (lockOwner, error) {
	var owner lockOwner
	data, err := readRegular(filepath.Join(path, "pid"), 2048)
	if err != nil {
		return owner, err
	}
	if err := json.Unmarshal(data, &owner); err != nil {
		return owner, err
	}
	start, startErr := strconv.ParseUint(owner.Start, 10, 64)
	if owner.PID <= 0 || !bootIdentity.MatchString(owner.Boot) || startErr != nil || strconv.FormatUint(start, 10) != owner.Start || !nonceIdentity.MatchString(owner.Token) {
		return owner, fmt.Errorf("invalid lock owner identity")
	}
	return owner, nil
}

func claimLock(path string, owner lockOwner) (bool, error) {
	err := os.Mkdir(path, 0700)
	if os.IsExist(err) {
		info, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("invalid lock directory")
		}
		current, readErr := readLockOwner(path)
		if readErr != nil {
			// Age and a bare PID cannot prove ownership ended. A missing or
			// corrupt identity stays refused for explicit operator inspection.
			return false, fmt.Errorf("lock-owner-unknown: %w", readErr)
		}
		alive, err := ownerAlive(current)
		if err != nil {
			return false, fmt.Errorf("lock-owner-unknown: %w", err)
		}
		if alive {
			return false, nil
		}
		latest, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		rechecked, err := readLockOwner(path)
		if err != nil || !os.SameFile(info, latest) || rechecked != current {
			return false, fmt.Errorf("lock owner changed during reclaim")
		}
		if err := os.RemoveAll(path); err != nil {
			return false, err
		}
		err = os.Mkdir(path, 0700)
		if err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return false, err
	}
	if err := writeAtomic(filepath.Join(path, "pid"), data); err != nil {
		_ = os.RemoveAll(path)
		return false, err
	}
	return true, nil
}
