//go:build linux

package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/goevery/sunstone/internal/modules/sunray/internal/adapters/google"
)

const secretAnnotation = "dev.sunstone.secret-env"

var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var versionName = regexp.MustCompile(`^projects/[A-Za-z0-9-]+/secrets/[A-Za-z0-9_-]+/versions/([1-9][0-9]*|[A-Za-z][A-Za-z0-9_-]*)$`)

// Feature orchestrates preparation and real-runtime delegation.
type Feature struct {
	runtime, state string
	source         *google.Adapter
	stdin          io.Reader
	stdout, stderr io.Writer
}

type snapshot struct {
	References map[string]string `json:"references"`
	Bundle     string            `json:"bundle"`
}

// New requires every host collaborator explicitly.
func New(runtime, state string, source *google.Adapter, stdin io.Reader, stdout, stderr io.Writer) (*Feature, error) {
	if !filepath.IsAbs(runtime) || !filepath.IsAbs(state) || source == nil || stdin == nil || stdout == nil || stderr == nil {
		return nil, errors.New("runtime, state directory, source, and standard IO are required")
	}

	return &Feature{runtime: runtime, state: state, source: source, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// Run implements the runc-compatible command boundary.
func (f *Feature) Run(ctx context.Context, args []string) (code int, resultErr error) {
	call, err := parse(args)
	if err != nil {
		writeError(args, err)

		return 1, err
	}
	defer func() {
		if resultErr != nil {
			writeError(args, resultErr)
		}
	}()
	if err := f.validateRuntime(); err != nil {
		return 1, err
	}
	if call.command != "create" && call.command != "exec" && call.command != "delete" && call.command != "run" && call.command != "checkpoint" && call.command != "restore" {
		return f.delegate(ctx, args)
	}
	if err := requireTmpfs(f.state); err != nil {
		return 1, err
	}
	if err := os.MkdirAll(f.state, 0700); err != nil {
		return 1, err
	}
	if err := privateDirectory(f.state); err != nil {
		return 1, err
	}
	digest := sha256.Sum256([]byte(call.root + "\x00" + call.id))
	directory := filepath.Join(f.state, hex.EncodeToString(digest[:]))
	if err := os.MkdirAll(directory, 0700); err != nil {
		return 1, err
	}
	unlock, err := lock(directory)
	if err != nil {
		return 1, err
	}
	defer unlock()
	snapshotPath := filepath.Join(directory, "snapshot.json")
	state, err := readSnapshot(snapshotPath)
	if err != nil {
		return 1, err
	}
	bundles := []string{}
	if state.Bundle != "" {
		bundles = append(bundles, state.Bundle)
	}
	if call.command == "create" && !slices.Contains(bundles, call.bundle) {
		bundles = append(bundles, call.bundle)
	}
	slices.Sort(bundles)
	for _, bundle := range bundles {
		digest := sha256.Sum256([]byte(bundle))
		bundleLock := filepath.Join(f.state, "bundle-"+hex.EncodeToString(digest[:]))
		if err := os.MkdirAll(bundleLock, 0700); err != nil {
			return 1, err
		}
		unlock, err := lock(bundleLock)
		if err != nil {
			return 1, err
		}
		defer unlock()
	}
	if state.Bundle != "" {
		if err := recoverBundle(directory, state.Bundle); err != nil {
			return 1, err
		}
	}
	if err := cleanupProcesses(directory); err != nil {
		return 1, err
	}
	switch call.command {
	case "delete":
		code, err = f.delegate(ctx, args)
		if code == 0 && err == nil {
			err = clearState(directory)
		}
		return code, err
	case "run", "checkpoint", "restore":
		if len(state.References) != 0 {
			return 1, errors.New("secret-enabled checkpoint, restore, and direct run are unsupported")
		}
		if call.command == "run" || call.command == "restore" {
			data, _, readErr := readRegular(filepath.Join(call.bundle, "config.json"))
			if readErr != nil {
				return 1, readErr
			}
			refs, _, readErr := bundleReferences(data)
			if readErr != nil {
				return 1, readErr
			}
			if len(refs) != 0 {
				return 1, errors.New("secret-enabled direct run and restore are unsupported")
			}
		}
		return f.delegate(ctx, args)
	case "exec":
		if len(state.References) == 0 {
			return f.delegate(ctx, args)
		}
		return f.exec(ctx, args, call, directory, state)
	}

	return f.create(ctx, args, call, directory, state)
}

func (f *Feature) create(ctx context.Context, args []string, call invocation, directory string, state snapshot) (code int, resultErr error) {
	snapshotPath := filepath.Join(directory, "snapshot.json")
	original, mode, err := readRegular(filepath.Join(call.bundle, "config.json"))
	if err != nil {
		return 1, err
	}
	references, document, err := bundleReferences(original)
	if err != nil {
		return 1, err
	}
	if len(references) == 0 {
		return f.delegate(ctx, args)
	}
	if call.debug {
		return 1, errors.New("debug logging is unsupported for secret-enabled operations")
	}
	if err := requireTmpfs(call.bundle); err != nil {
		return 1, err
	}
	if err := requireTmpfs(call.root); err != nil {
		return 1, err
	}
	values, resolved, err := f.fetch(ctx, references)
	if err != nil {
		return 1, err
	}
	process, err := inject(document["process"], values)
	if err != nil {
		return 1, err
	}
	document["process"] = process
	modified, err := json.Marshal(document)
	if err != nil {
		return 1, errors.New("encode OCI bundle")
	}
	// Journal only the unmodified specification before exposing any payload.
	// A later invocation restores it after uncatchable interruption.
	journal := backup{Config: original, Mode: mode}
	journalData, _ := json.Marshal(journal)
	if err := replace(filepath.Join(directory, "original.json"), journalData, 0600); err != nil {
		return 1, err
	}
	previousState := state
	state = snapshot{References: resolved, Bundle: call.bundle}
	stateData, _ := json.Marshal(state)
	if err := replace(snapshotPath, stateData, 0600); err != nil {
		return 1, err
	}
	defer func() {
		restoreErr := recoverBundle(directory, call.bundle)
		resultErr = errors.Join(resultErr, restoreErr)
		// Retain the snapshot-to-journal association if restoration itself
		// failed, so the next invocation cannot read an injected bundle as original.
		if restoreErr != nil {
			return
		}

		if code != 0 || resultErr != nil {
			if len(previousState.References) != 0 {
				data, _ := json.Marshal(previousState)
				resultErr = errors.Join(resultErr, replace(snapshotPath, data, 0600))
			} else {
				resultErr = errors.Join(resultErr, removeIfExists(snapshotPath))
			}
		}
	}()
	if err := replace(filepath.Join(call.bundle, "config.json"), modified, 0600); err != nil {
		return 1, err
	}

	return f.delegate(ctx, args)
}

func (f *Feature) validateRuntime() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	realInfo, err := os.Stat(f.runtime)
	if err != nil {
		return fmt.Errorf("inspect real runtime: %w", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return err
	}
	if !realInfo.Mode().IsRegular() || realInfo.Mode().Perm()&0111 == 0 || os.SameFile(realInfo, selfInfo) {
		return errors.New("real runtime must be another executable file")
	}

	return nil
}

func (f *Feature) fetch(ctx context.Context, references map[string]string) (map[string]string, map[string]string, error) {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return f.source.Fetch(ctx, references)
}

func bundleReferences(data []byte) (map[string]string, map[string]json.RawMessage, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil || document == nil {
		return nil, nil, errors.New("invalid OCI bundle JSON")
	}
	var annotations map[string]string
	if raw := document["annotations"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &annotations); err != nil {
			return nil, nil, errors.New("invalid OCI annotations")
		}
	}
	value, found := annotations[secretAnnotation]
	if !found {
		return nil, document, nil
	}
	references, err := decodeReferences([]byte(value))
	return references, document, err
}

func decodeReferences(data []byte) (map[string]string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("secret annotation must be a nonempty JSON object")
	}
	references := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid secret annotation")
		}
		name, ok := token.(string)
		var reference string
		if !ok || decoder.Decode(&reference) != nil || !environmentName.MatchString(name) || !versionName.MatchString(reference) || references[name] != "" {
			return nil, errors.New("invalid or duplicate environment name or secret version reference")
		}
		references[name] = reference
	}
	if _, err := decoder.Token(); err != nil {
		return nil, errors.New("invalid secret annotation")
	}
	if _, err := decoder.Token(); err != io.EOF || len(references) == 0 {
		return nil, errors.New("invalid or empty secret annotation")
	}
	return references, nil
}

func inject(data []byte, values map[string]string) ([]byte, error) {
	var process map[string]json.RawMessage
	if err := json.Unmarshal(data, &process); err != nil || process == nil {
		return nil, errors.New("OCI specification requires a process")
	}
	var environment []string
	if raw := process["env"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &environment); err != nil {
			return nil, errors.New("invalid OCI process environment")
		}
	}
	merged := make([]string, 0, len(environment)+len(values))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := values[name]; !found {
			merged = append(merged, entry)
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		merged = append(merged, name+"="+values[name])
	}
	process["env"], _ = json.Marshal(merged)

	return json.Marshal(process)
}

func (f *Feature) exec(ctx context.Context, args []string, call invocation, directory string, state snapshot) (int, error) {
	if call.debug {
		return 1, errors.New("debug logging is unsupported for secret-enabled operations")
	}
	if call.processIndex < 0 || call.process == "" {
		return 1, errors.New("secret-enabled exec requires a separate --process specification")
	}
	original, _, err := readRegular(call.process)
	if err != nil {
		return 1, err
	}
	values, resolved, err := f.fetch(ctx, state.References)
	if err != nil {
		return 1, err
	}
	for name, reference := range state.References {
		if resolved[name] != reference {
			return 1, errors.New("exec secret version does not match container snapshot")
		}
	}
	modified, err := inject(original, values)
	if err != nil {
		return 1, err
	}
	file, err := os.CreateTemp(directory, "process-*.json")
	if err != nil {
		return 1, err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(modified); err != nil {
		file.Close()
		return 1, err
	}
	if err := file.Close(); err != nil {
		return 1, err
	}
	delegated := slices.Clone(args)
	if call.processInline {
		name, _, _ := strings.Cut(delegated[call.processIndex], "=")
		delegated[call.processIndex] = name + "=" + file.Name()
	} else {
		delegated[call.processIndex] = file.Name()
	}
	return f.delegate(ctx, delegated)
}
