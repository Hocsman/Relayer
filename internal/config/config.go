// Package config loads and creates Relayer interception configuration files.
// It deliberately owns YAML concerns so the interception engine remains
// independent from persistence and command-line conventions.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Hocsman/Relayer/internal/intercept"
	"gopkg.in/yaml.v3"
)

const DefaultPath = "config.yaml"

// Result describes the effective interception configuration and whether the
// loader had to create the file during this call.
type Result struct {
	Patterns []intercept.Pattern
	Created  bool
}

// ConfigPattern is the strict YAML representation exposed to users.
type ConfigPattern struct {
	Pattern     string `yaml:"pattern"`
	Description string `yaml:"description"`
}

type file struct {
	InterceptPatterns []ConfigPattern `yaml:"intercept_patterns"`
}

// Load reads path before any PTY is started. It accepts both a direct list and
// the intercept_patterns wrapper documented in the README. A missing file is
// populated atomically with the built-in defaults.
func Load(path string) (Result, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, errors.New("le chemin du fichier de configuration est vide")
	}

	data, err := os.ReadFile(path)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		created, err = createDefault(path)
		if err != nil {
			return Result{}, err
		}
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return Result{}, fmt.Errorf("lecture de %s: %w", path, err)
	}

	configured, err := decode(data)
	if err != nil {
		return Result{}, fmt.Errorf("configuration %s invalide: %w", path, err)
	}
	patterns, err := validate(configured)
	if err != nil {
		return Result{}, fmt.Errorf("configuration %s invalide: %w", path, err)
	}
	return Result{Patterns: patterns, Created: created}, nil
}

// DefaultPatterns returns an independent copy of the built-in patterns.
func DefaultPatterns() []intercept.Pattern {
	return intercept.DefaultPatterns()
}

func createDefault(path string) (bool, error) {
	defaults := intercept.DefaultPatterns()
	configured := make([]ConfigPattern, 0, len(defaults))
	for _, pattern := range defaults {
		configured = append(configured, ConfigPattern{
			Pattern:     pattern.Expression,
			Description: pattern.Description,
		})
	}
	payload, err := yaml.Marshal(file{InterceptPatterns: configured})
	if err != nil {
		return false, fmt.Errorf("sérialisation de la configuration par défaut: %w", err)
	}
	payload = append([]byte("# Patterns d'interception de Relayer.\n"), payload...)

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return false, fmt.Errorf("création du dossier de configuration %s: %w", directory, err)
	}

	// Publish only a fully written file. Link never replaces an existing user
	// configuration, even if another process creates it concurrently.
	temporary, err := os.CreateTemp(directory, ".relayer-config-*.tmp")
	if err != nil {
		return false, fmt.Errorf("création temporaire de %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("permissions de %s: %w", temporaryPath, err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("écriture de %s: %w", temporaryPath, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("synchronisation de %s: %w", temporaryPath, err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("fermeture de %s: %w", temporaryPath, err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		// Filesystems without hard links keep the no-overwrite guarantee via an
		// exclusive direct creation.
		return createExclusively(path, payload, err)
	}
	return true, nil
}

func createExclusively(path string, payload []byte, linkErr error) (bool, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("publication de %s (lien indisponible: %v): %w", path, linkErr, err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if _, err := file.Write(payload); err != nil {
		cleanup()
		return false, fmt.Errorf("écriture de %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("synchronisation de %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return false, fmt.Errorf("fermeture de %s: %w", path, err)
	}
	return true, nil
}

func decode(data []byte) ([]ConfigPattern, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 {
		return nil, errors.New("le document YAML est vide")
	}

	root := document.Content[0]
	if err := validateScalarTypes(root); err != nil {
		return nil, err
	}
	switch root.Kind {
	case yaml.SequenceNode:
		var direct []ConfigPattern
		if err := decodeStrict(data, &direct); err != nil {
			return nil, err
		}
		return direct, nil
	case yaml.MappingNode:
		var wrapped file
		if err := decodeStrict(data, &wrapped); err != nil {
			return nil, err
		}
		return wrapped.InterceptPatterns, nil
	default:
		return nil, errors.New("la racine YAML doit être une liste ou contenir intercept_patterns")
	}
}

func validateScalarTypes(root *yaml.Node) error {
	sequence := root
	if root.Kind == yaml.MappingNode {
		sequence = nil
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Value == "intercept_patterns" {
				sequence = root.Content[index+1]
				break
			}
		}
	}
	if sequence == nil || sequence.Kind != yaml.SequenceNode {
		return nil
	}

	for entryIndex, entry := range sequence.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		for fieldIndex := 0; fieldIndex+1 < len(entry.Content); fieldIndex += 2 {
			name := entry.Content[fieldIndex].Value
			if name != "pattern" && name != "description" {
				continue
			}
			value := entry.Content[fieldIndex+1]
			if value.Kind == yaml.AliasNode && value.Alias != nil {
				value = value.Alias
			}
			if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return fmt.Errorf("entrée %d: %s doit être une chaîne YAML", entryIndex+1, name)
			}
		}
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("plusieurs documents YAML ne sont pas autorisés")
		}
		return err
	}
	return nil
}

func validate(configured []ConfigPattern) ([]intercept.Pattern, error) {
	if len(configured) == 0 {
		return nil, errors.New("aucun pattern d'interception n'est défini")
	}

	patterns := make([]intercept.Pattern, 0, len(configured))
	for index, pattern := range configured {
		if strings.TrimSpace(pattern.Pattern) == "" {
			return nil, fmt.Errorf("entrée %d: pattern manquant", index+1)
		}
		if strings.TrimSpace(pattern.Description) == "" {
			return nil, fmt.Errorf("entrée %d: description manquante", index+1)
		}
		if _, err := regexp.Compile(pattern.Pattern); err != nil {
			return nil, fmt.Errorf("entrée %d: regex invalide: %w", index+1, err)
		}
		patterns = append(patterns, intercept.Pattern{
			Name:        inferName(pattern, index),
			Description: strings.TrimSpace(pattern.Description),
			Expression:  pattern.Pattern,
			Sensitive:   isSensitive(pattern),
		})
	}
	return patterns, nil
}

func inferName(pattern ConfigPattern, index int) string {
	text := strings.ToLower(pattern.Pattern + " " + pattern.Description)
	switch {
	case isSensitive(pattern):
		return "password"
	case strings.Contains(text, "overwrite"), strings.Contains(text, "écras"):
		return "overwrite"
	case strings.Contains(text, "continue"), strings.Contains(text, "poursuite"):
		return "continue"
	case strings.Contains(text, "confirm"), strings.Contains(text, "oui/non"):
		return "confirmation"
	default:
		return fmt.Sprintf("config-%d", index+1)
	}
}

func isSensitive(pattern ConfigPattern) bool {
	return intercept.IsSensitiveText(pattern.Pattern + " " + pattern.Description)
}
