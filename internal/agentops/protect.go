package agentops

import (
	"errors"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

// ErrProtected is returned by CreateGuarded and UpdateGuarded when the file
// would land in a protected directory. Nothing is written, and no directory is
// created, in that case.
var ErrProtected = errors.New("the destination is inside a protected directory")

// ErrSymlinkedFile is returned by CreateGuarded and UpdateGuarded when the agent
// file's own name is a symlink: where it points is not something a guarded write
// can judge, so it is not written through. A symlinked directory above the file
// is followed as it always was.
var ErrSymlinkedFile = errors.New("the agent file is a symlink")

// CreateGuarded is Create for a caller that must not write into the protected
// directories (real paths), such as the `mux mcp` Tether plants into a launched
// agent: the catalog root, the run directory and the state directory are the
// operator's, not the agent's. With no protected directories it is Create.
//
// The decision is made on the directory the write goes into, not on a path
// that can be re-pointed afterwards. A check that resolves a path and then
// writes it later loses to a symlink flipped between the two (an agent with a
// writable repository root can run that loop from its shell), so on Linux the
// destination directory is opened once, judged by its identity and its
// ancestors', and the file is created relative to that open directory. Elsewhere
// the check is on the resolved path and the race is narrowed, not closed; the
// protection is applied on Linux only (CW-20261001-0138).
func CreateGuarded(layerRoot, id string, p Params, protected []string) (string, error) {
	if len(protected) == 0 {
		return Create(layerRoot, id, p)
	}
	if !ValidID(id) {
		return "", invalidIDError(id)
	}
	path := PathFor(layerRoot, id)
	body, err := marshalAgent(newAgent(id, p))
	if err != nil {
		return "", err
	}
	if err := createFileGuarded(path, body, protected); err != nil {
		return "", err
	}
	return path, nil
}

// UpdateGuarded is Update with the same guarantee as CreateGuarded: the file is
// read and rewritten through the directory it was judged in, and never through
// a symlink in its final component. With no protected directories it is Update.
func UpdateGuarded(path string, p Params, protected []string) (launchprofile.LaunchProfile, error) {
	if len(protected) == 0 {
		return Update(path, p)
	}
	return updateFileGuarded(path, p, protected)
}
