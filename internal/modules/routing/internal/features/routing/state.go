package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type stateDocument struct {
	Version int   `json:"version"`
	Route   Route `json:"route"`
}

func loadState(path string) (*Route, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open routing state: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var document stateDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode routing state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("routing state must contain one JSON value")
	}
	if document.Version != stateVersion {
		return nil, fmt.Errorf("unsupported routing state version %d", document.Version)
	}
	return &document.Route, nil
}

func persistState(path string, route Route) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".sunbeam-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(stateDocument{Version: stateVersion, Route: route}); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}
