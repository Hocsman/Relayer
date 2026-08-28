package app

import (
	"errors"
	"flag"
	"io"
	"strings"
	"unicode"

	"github.com/Hocsman/Relayer/internal/config"
)

type options struct {
	pane1      string
	pane2      string
	configPath string
	pane1Set   bool
	pane2Set   bool
}

func parseOptions(arguments []string, diagnostics io.Writer) (options, error) {
	flags := flag.NewFlagSet("relayer", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	pane1 := flags.String(
		"pane1",
		"",
		"[deprecated] replaces the first agent's direct command (quotes accepted, no implicit shell)",
	)
	pane2 := flags.String(
		"pane2",
		"",
		"[deprecated] replaces the second agent's direct command (quotes accepted, no implicit shell)",
	)
	configPath := flags.String("config", config.DefaultPath, "YAML configuration file")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}

	result := options{
		pane1:      *pane1,
		pane2:      *pane2,
		configPath: *configPath,
	}
	flags.Visit(func(current *flag.Flag) {
		switch current.Name {
		case "pane1":
			result.pane1Set = true
		case "pane2":
			result.pane2Set = true
		}
	})
	return result, nil
}

// splitLegacyCommand turns a deprecated --paneN string into an argv vector.
// It supports whitespace, single/double quotes and backslash escaping, but it
// deliberately does not interpret variables, globbing, redirections, pipes,
// command substitutions or any other shell syntax.
func splitLegacyCommand(value string) ([]string, error) {
	var (
		arguments    []string
		current      strings.Builder
		quote        rune
		escaped      bool
		tokenStarted bool
	)

	flush := func() {
		if !tokenStarted {
			return
		}
		arguments = append(arguments, current.String())
		current.Reset()
		tokenStarted = false
	}

	for _, character := range value {
		if escaped {
			current.WriteRune(character)
			tokenStarted = true
			escaped = false
			continue
		}

		switch quote {
		case '\'':
			if character == '\'' {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			tokenStarted = true
		case '"':
			switch character {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				current.WriteRune(character)
			}
			tokenStarted = true
		default:
			switch {
			case unicode.IsSpace(character):
				flush()
			case character == '\'' || character == '"':
				quote = character
				tokenStarted = true
			case character == '\\':
				escaped = true
				tokenStarted = true
			default:
				current.WriteRune(character)
				tokenStarted = true
			}
		}
	}

	if escaped {
		return nil, errors.New("incomplete command: trailing escape character")
	}
	if quote != 0 {
		return nil, errors.New("incomplete command: unclosed quote")
	}
	flush()
	return arguments, nil
}
