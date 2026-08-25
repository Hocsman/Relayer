package app

import "strings"

const defaultMockCommand = `exec bash -c 'echo "🤖 Agent démarré..."; for i in {1..20}; do echo "Génération ligne $i..."; sleep 0.1; done; echo "⚠️ Attention: Overwrite file? [Y/n]"; IFS= read -r ans; echo "✅ Vous avez répondu : $ans. Fin de la tâche."'`

func resolvePaneCommand(command string) (resolved string, mock bool) {
	if strings.TrimSpace(command) == "" {
		return defaultMockCommand, true
	}
	return command, false
}
