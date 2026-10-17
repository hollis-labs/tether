package workerruntime

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func ServiceDefinition(platform, binary, root, _ string) (string, error) {
	switch platform {
	case "linux":
		quote := func(s string) string {
			return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(s) + "\""
		}
		return "[Unit]\nDescription=Tether thin execution outpost\nAfter=network-online.target\n[Service]\nExecStart=" + quote(binary) + " --root " + quote(root) + " run\nRestart=always\nRestartSec=2\nKillMode=process\n[Install]\nWantedBy=default.target\n", nil
	case "darwin":
		esc := html.EscapeString
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.tether.worker</string>
<key>ProgramArguments</key><array><string>%s</string><string>--root</string><string>%s</string><string>run</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, esc(binary), esc(root), esc(filepath.Join(root, "worker.log")), esc(filepath.Join(root, "worker.log"))), nil
	default:
		return "", fmt.Errorf("service install unsupported on %s", platform)
	}
}
func InstallService(ctx context.Context, root, binary string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cfg, err := Load(root)
	if err != nil {
		return err
	}
	definition, err := ServiceDefinition(runtime.GOOS, binary, root, cfg.Path)
	if err != nil {
		return err
	}
	var path string
	if runtime.GOOS == "linux" {
		path = filepath.Join(home, ".config", "systemd", "user", "tether-outpost.service")
	} else {
		path = filepath.Join(home, "Library", "LaunchAgents", "dev.tether.worker.plist")
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err = os.WriteFile(path, []byte(definition), 0600); err != nil { //nolint:gosec // G703: path is the unit file under the worker user's own home
		return err
	}
	var command *exec.Cmd
	if runtime.GOOS == "linux" {
		if err := exec.CommandContext(ctx, "systemctl", "--user", "daemon-reload").Run(); err != nil {
			return err
		}
		command = exec.CommandContext(ctx, "systemctl", "--user", "enable", "--now", "tether-outpost.service")
	} else {
		command = exec.CommandContext(ctx, "launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path) //nolint:gosec // G204: path is the unit file this command just wrote
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}
