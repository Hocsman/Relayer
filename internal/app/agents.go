package app

import (
	"fmt"
	"strings"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
)

const defaultMockScript = `echo "🤖 Agent démarré..."; for i in {1..20}; do echo "Génération ligne $i..."; sleep 0.1; done; echo "⚠️ Attention: Overwrite file? [Y/n]"; IFS= read -r ans; echo "✅ Vous avez répondu : $ans. Fin de la tâche."`

type agentResolution struct {
	Specs          []agent.Spec
	MockAgentNames []string
	Warnings       []string
}

// resolveAgentPlans applies the deprecated CLI overrides to the already
// validated YAML configuration. An empty configured agent list deliberately
// selects the historical two-agent demonstration.
func resolveAgentPlans(configuration config.Result, cli options, workingDirectory string) (agentResolution, error) {
	backend := configuration.Backend
	if strings.TrimSpace(backend) == "" {
		backend = agent.BackendPTY
	}

	result := agentResolution{
		Specs: append([]agent.Spec(nil), configuration.Agents...),
	}
	if len(result.Specs) == 0 {
		result.Specs = defaultAgentSpecs(workingDirectory, backend)
		result.MockAgentNames = []string{result.Specs[0].Name, result.Specs[1].Name}
	}

	for _, override := range []struct {
		set   bool
		value string
		index int
		flag  string
	}{
		{set: cli.pane1Set, value: cli.pane1, index: 0, flag: "--pane1"},
		{set: cli.pane2Set, value: cli.pane2, index: 1, flag: "--pane2"},
	} {
		if !override.set {
			continue
		}
		if override.index >= len(result.Specs) {
			return agentResolution{}, fmt.Errorf(
				"%s cannot replace agent %d: the configuration only contains %d agent(s)",
				override.flag,
				override.index+1,
				len(result.Specs),
			)
		}

		spec := &result.Specs[override.index]
		removeName(&result.MockAgentNames, spec.Name)
		if strings.TrimSpace(override.value) == "" {
			spec.Command = mockCommand()
			spec.Shell = ""
			result.MockAgentNames = append(result.MockAgentNames, spec.Name)
		} else {
			arguments, err := splitLegacyCommand(override.value)
			if err != nil {
				return agentResolution{}, fmt.Errorf("invalid %s: %w", override.flag, err)
			}
			if len(arguments) == 0 {
				return agentResolution{}, fmt.Errorf("%s contains no command", override.flag)
			}
			spec.Command = arguments
			spec.Shell = ""
		}
		result.Warnings = append(result.Warnings,
			"Avertissement: "+override.flag+" est obsolète; configurez agents dans config.yaml.",
			"Compatibilité: "+override.flag+" est exécuté en argv direct, sans interprétation par un shell.",
		)
	}

	validated, err := agent.ValidateAll(result.Specs, workingDirectory, backend)
	if err != nil {
		return agentResolution{}, fmt.Errorf("invalid effective agent configuration: %w", err)
	}
	if len(validated) == 0 || len(validated) > 8 {
		return agentResolution{}, fmt.Errorf("relayer supports between 1 and 8 agents, got: %d", len(validated))
	}
	result.Specs = validated
	return result, nil
}

func defaultAgentSpecs(workingDirectory, backend string) []agent.Spec {
	return []agent.Spec{
		{
			ID:      "demo-a",
			Name:    "Agent A (Claude)",
			Command: mockCommand(),
			Cwd:     workingDirectory,
			Adapter: agent.AdapterGeneric,
			Backend: backend,
		},
		{
			ID:      "demo-b",
			Name:    "Agent B (Local)",
			Command: mockCommand(),
			Cwd:     workingDirectory,
			Adapter: agent.AdapterGeneric,
			Backend: backend,
		},
	}
}

func mockCommand() []string {
	return []string{"bash", "-c", defaultMockScript}
}

func removeName(names *[]string, name string) {
	filtered := (*names)[:0]
	for _, candidate := range *names {
		if candidate != name {
			filtered = append(filtered, candidate)
		}
	}
	*names = filtered
}
