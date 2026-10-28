//go:build linux

package security

import (
	"net/http"
	"strings"
)

// Command returns a safe category only; arbitrary arguments and payloads are never retained.
func Command(args []string) (string, bool) {
	if len(args) == 0 {
		return "help", false
	}
	tokens := []string{}
	valueFlags := map[string]bool{"--project": true, "--target": true, "--format": true, "-f": true, "--config": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(tokens) > 0 {
			if arg == "--" {
				break
			}
			if strings.HasPrefix(arg, "-") {
				if tokens[0] == "info" && strings.HasPrefix(arg, "--show-log") {
					return "info_log", true
				}
				continue
			}
			tokens = append(tokens, arg)
			continue
		}
		if strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++
				continue
			}
			if arg == "--help" || arg == "-h" || arg == "--version" {
				continue
			}
			if strings.Contains(arg, "=") {
				continue
			}
			switch arg {
			case "--force-local", "--debug", "--verbose", "--quiet", "-q", "--all", "--sub-commands":
				continue
			}
			return "unknown_option", true
		}
		tokens = append(tokens, arg)
	}
	if len(tokens) == 0 {
		return "help_or_version", false
	}
	root := tokens[0]
	if len(tokens) > 1 {
		switch root {
		case "config", "profile", "storage", "network", "image", "project", "remote", "operation":
			sub := tokens[1]
			if root == "storage" && sub == "volume" && len(tokens) > 2 {
				sub = tokens[2]
			}
			switch sub {
			case "list", "ls", "show", "get", "info":
				return root + "_read", false
			}
		}
	}
	safe := map[string]bool{"list": true, "ls": true, "info": true, "version": true, "help": true, "help_or_version": true, "completion": true, "config_read": true, "profile_read": true, "storage_read": true, "network_read": true, "image_read": true, "project_read": true, "remote_read": true, "operation_read": true}
	if safe[root] {
		return root, false
	}
	known := map[string]bool{"info_log": true, "exec": true, "shell": true, "console": true, "file": true, "export": true, "publish": true, "copy": true, "cp": true, "move": true, "mv": true, "config": true, "profile": true, "storage": true, "network": true, "query": true, "start": true, "restart": true, "restore": true, "snapshot": true, "delete": true, "launch": true, "init": true, "admin": true, "operation": true, "stop": true, "pause": true, "import": true, "image": true, "project": true, "remote": true, "alias": true}
	if known[root] {
		return root, true
	}
	return "alias_or_unknown", true
}

// AllowedWhileLocked is an exact allowlist; read methods alone do not imply safety.
func AllowedWhileLocked(method, path string) bool {
	if method == http.MethodGet {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		if len(parts) == 4 && parts[0] == "internal" && parts[1] == "containers" && (parts[3] == "onstop" || parts[3] == "onstopns") {
			return true
		}
		switch path {
		case "/1.0", "/1.0/payincus", "/1.0/payincus/report", "/1.0/payincus/security", "/1.0/payincus/security/report":
			return true
		}
	}
	if method == http.MethodPost {
		switch path {
		case "/1.0/payincus/security/ack", "/1.0/payincus/security/policy", "/1.0/payincus/security/recover", "/1.0/payincus/security/lock", "/1.0/payincus/security/unlock", "/1.0/payincus/security/resume":
			return true
		}
	}
	return false
}

// SensitivePath excludes the entire management subtree from request-body debug capture.
func SensitivePath(path string) bool {
	return path == "/1.0/payincus/security" || strings.HasPrefix(path, "/1.0/payincus/security/")
}
