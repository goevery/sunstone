//go:build linux

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type invocation struct {
	command, id, bundle, process, root, log string
	processIndex                            int
	processInline                           bool
	debug                                   bool
}

// Recognize runc 1.2's options without reinterpreting option values as commands.
// Unknown options fail closed rather than silently bypassing preparation.
func parse(args []string) (invocation, error) {
	call := invocation{root: "/run/runc", bundle: ".", processIndex: -1}
	index := 0
	for index < len(args) && strings.HasPrefix(args[index], "-") {
		name, value, inline := strings.Cut(args[index], "=")
		switch name {
		case "--help", "-h", "--version", "-v":
			return call, nil
		case "--debug":
			call.debug = true
		case "--systemd-cgroup":
		case "--root", "--log", "--log-format", "--rootless", "--criu":
			if !inline {
				index++
				if index >= len(args) {
					return call, errors.New("missing global option value")
				}
				value = args[index]
			}
			if name == "--root" {
				call.root = value
			}
			if name == "--log" {
				call.log = value
			}
		default:
			return call, errors.New("unsupported runtime global option")
		}
		index++
	}
	if index >= len(args) {
		return call, nil
	}
	call.command = args[index]
	index++
	if call.command != "create" && call.command != "exec" && call.command != "delete" && call.command != "run" && call.command != "checkpoint" && call.command != "restore" {
		return call, nil
	}
	for index < len(args) {
		argument := args[index]
		if argument == "--" {
			index++
			break
		}
		if !strings.HasPrefix(argument, "-") {
			break
		}
		name, value, inline := strings.Cut(argument, "=")
		switch name {
		case "--help", "-h":
			call.command = ""
			return call, nil
		case "--detach", "-d", "--no-pivot", "--no-new-keyring", "--force", "-f", "--tty", "-t", "--no-new-privs", "--ignore-paused":
		case "--bundle", "-b", "--process", "-p", "--console-socket", "--pidfd-socket", "--pid-file", "--preserve-fds", "--cwd", "--env", "-e", "--user", "-u", "--additional-gids", "-g", "--process-label", "--apparmor", "--cap", "-c", "--cgroup":
			optionIndex := index
			if !inline {
				index++
				if index >= len(args) {
					return call, errors.New("missing runtime option value")
				}
				value = args[index]
			}
			if name == "--bundle" || name == "-b" {
				call.bundle = value
			}
			if name == "--process" || name == "-p" {
				call.process = value
				call.processIndex = index
				call.processInline = inline
				if inline {
					call.processIndex = optionIndex
				}
			}
		default:
			return call, errors.New("unsupported runtime command option")
		}
		index++
	}
	if index >= len(args) {
		return call, errors.New("container ID is required")
	}
	call.id = args[index]
	if call.id == "" || strings.ContainsAny(call.id, "/\x00\n") || call.id == "." || call.id == ".." {
		return call, errors.New("invalid container ID")
	}
	var err error
	call.root, err = canonicalPath(call.root)
	if err != nil {
		return call, err
	}
	call.bundle, err = canonicalPath(call.bundle)

	return call, err
}

// Resolve existing ancestors too, so aliases to one bundle/root share locks and
// snapshots even before runc has created the final state directory.
func canonicalPath(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}
