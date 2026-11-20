package runtoken

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const handoffSeals = unix.F_SEAL_WRITE | unix.F_SEAL_GROW | unix.F_SEAL_SHRINK | unix.F_SEAL_SEAL

func memoryHandoff(data []byte) (string, func(), error) {
	fd, err := unix.MemfdCreate("tether-run-handoff", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return "", nil, ErrHandoff
	}
	f := os.NewFile(uintptr(fd), "run-handoff") //nolint:gosec // G115: fd comes from successful memfd_create.
	release := func() { _ = f.Close() }
	if unix.Fchmod(fd, 0600) != nil {
		release()
		return "", nil, ErrHandoff
	}
	if _, err := f.Write(data); err != nil {
		release()
		return "", nil, ErrHandoff
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, handoffSeals); err != nil {
		release()
		return "", nil, ErrHandoff
	}
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fd), release, nil
}
func readMemoryHandoff(path string) ([]byte, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "proc" || parts[3] != "fd" {
		return nil, ErrHandoff
	}
	pid, e1 := strconv.Atoi(parts[2])
	fd, e2 := strconv.Atoi(parts[4])
	if e1 != nil || e2 != nil || pid <= 0 || fd < 0 || path != fmt.Sprintf("/proc/%d/fd/%d", pid, fd) {
		return nil, ErrHandoff
	}
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0) //nolint:gosec // G304: canonical /proc/<pid>/fd/<fd>, validated below as sealed anonymous memory.
	if err != nil {
		return nil, ErrHandoff
	}
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Nlink != 0 || int(st.Uid) != os.Geteuid() {
		return nil, ErrHandoff
	} //nolint:gosec // G115: os.File descriptor is a small non-negative fd.
	seals, err := unix.FcntlInt(f.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&handoffSeals != handoffSeals {
		return nil, ErrHandoff
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxSize+2049))
	if err != nil || len(data) > MaxSize+2048 {
		return nil, ErrHandoff
	}
	return data, nil
}

// HandoffSupported probes the trusted launcher context without any secret data.
func HandoffSupported() bool {
	path, release, err := memoryHandoff([]byte("probe"))
	if err != nil {
		return false
	}
	defer release()
	_, err = readMemoryHandoff(path)
	return err == nil
}
