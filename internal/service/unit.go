package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

const UnitName = "tether-worker.service"
const RuntimeRootEnv = "TETHER_SERVICE_RUNTIME_ROOT"

func quoteUnit(value string) (string, error) {
	for _, c := range value {
		if unicode.IsControl(c) {
			return "", fmt.Errorf("unit value contains a control character")
		}
	}
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(value)
	return "\"" + value + "\"", nil
}

func quoteExec(value string) (string, error) {
	quoted, err := quoteUnit(value)
	return strings.ReplaceAll(quoted, "$", "$$"), err
}

func renderUnit(root, catalog, pathEnv string) ([]byte, error) {
	if len(pathEnv) > 16<<10 {
		return nil, fmt.Errorf("installing shell PATH exceeds 16 KiB")
	}
	var args []string
	for _, value := range []string{filepath.Join(root, "current", "tether"), "--catalog", catalog, "serve"} {
		quoted, err := quoteExec(value)
		if err != nil {
			return nil, err
		}
		args = append(args, quoted)
	}
	path, err := quoteUnit("PATH=" + pathEnv)
	if err != nil {
		return nil, err
	}
	provenance, err := quoteUnit(RuntimeRootEnv + "=" + root)
	if err != nil {
		return nil, err
	}
	// Shim-hosted agents remain in their independently owned transient units.
	// There is deliberately no PartOf/BindsTo relationship or shim stop command.
	return []byte("[Unit]\nDescription=Tether worker daemon\nStartLimitIntervalSec=60\nStartLimitBurst=5\n\n[Service]\nType=exec\nExecStart=" + strings.Join(args, " ") + "\nEnvironment=" + path + "\nEnvironment=" + provenance + "\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=30\nKillMode=control-group\n\n[Install]\nWantedBy=default.target\n"), nil
}
