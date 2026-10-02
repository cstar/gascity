package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/gastownhall/gascity/internal/fsys"
)

// controlReadyFileInput describes a resolved opening input. Collection belongs
// at the opening boundary; secrets are hashed, never logged by the registry.
type controlReadyFileInput struct {
	Exists  bool
	Mode    uint32
	Content []byte
}

func controlReadyCollectFiles(f fsys.FS, paths []string) (map[string]controlReadyFileInput, error) {
	inputs := make(map[string]controlReadyFileInput, len(paths))
	for _, path := range paths {
		info, err := f.Stat(path)
		if os.IsNotExist(err) {
			inputs[path] = controlReadyFileInput{}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat readiness opening input %s: %w", path, err)
		}
		content, err := f.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read readiness opening input %s: %w", path, err)
		}
		inputs[path] = controlReadyFileInput{Exists: true, Mode: uint32(info.Mode()), Content: content}
	}
	return inputs, nil
}

func controlReadyOpeningFingerprint(env map[string]string, config []byte, files map[string]controlReadyFileInput) string {
	h := sha256.New()
	// Length-prefix fields so different key/value boundaries cannot alias.
	field := func(value []byte) {
		_, _ = h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(value))))
		_, _ = h.Write(value)
	}
	field([]byte("control-ready-opening-v1"))
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	field(binary.BigEndian.AppendUint64(nil, uint64(len(keys))))
	for _, key := range keys {
		field([]byte(key))
		field([]byte(env[key]))
	}
	field(config)
	keys = keys[:0]
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	field(binary.BigEndian.AppendUint64(nil, uint64(len(keys))))
	for _, key := range keys {
		input := files[key]
		field([]byte(key))
		present := byte(0)
		if input.Exists {
			present = 1
		}
		field([]byte{present})
		field(binary.BigEndian.AppendUint32(nil, input.Mode))
		field(input.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}
