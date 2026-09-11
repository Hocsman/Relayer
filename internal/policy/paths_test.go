package policy

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestIsSensitivePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{".envrc", true},
		{"path/to/.env", true},
		{"sub/dir/.env.test", true},
		{"id_rsa", true},
		{"id_ed25519", true},
		{"id_ecdsa", true},
		{"id_dsa", true},
		{"~/.ssh/id_rsa", true},
		{"~/.ssh/authorized_keys", true},
		{"~/.ssh/known_hosts", true},
		{"/home/user/.ssh/config", true},
		{"C:\\Users\\user\\.ssh\\id_rsa", true},
		{"~/.aws/credentials", true},
		{"~/.aws/config", true},
		{"~/.kube/config", true},
		{".git/config", true},
		{"/etc/shadow", true},
		{"/etc/sudoers", true},
		{"credentials", true},
		{"service-account-prod.json", true},
		{"client_secret_xyz.json", true},
		// Harmless paths
		{"main.go", false},
		{"package.json", false},
		{"src/components/App.tsx", false},
		{"README.md", false},
		{"docs/environment.md", false},
		{"env_test.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := IsSensitivePath(tt.path)
			if got != tt.want {
				t.Errorf("IsSensitivePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsPathInsideWorkspace(t *testing.T) {
	ws := filepath.Clean("/workspace/project")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"relative inside", "src/index.ts", true},
		{"relative with dot", "./src/index.ts", true},
		{"relative clean subpath", "a/b/c/file.txt", true},
		{"relative up and down inside", "src/../src/index.ts", true},
		{"relative escaping up", "../secret.txt", false},
		{"relative escaping double up", "../../etc/shadow", false},
		{"relative up and outside", "src/../../outside.txt", false},
		{"absolute inside", filepath.Join(ws, "src/index.ts"), true},
		{"absolute outside", filepath.Clean("/workspace/other/file.txt"), false},
		{"absolute root", filepath.Clean("/etc/shadow"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPathInsideWorkspace(tt.path, ws)
			if got != tt.want {
				t.Errorf("IsPathInsideWorkspace(%q, %q) = %v, want %v", tt.path, ws, got, tt.want)
			}
		})
	}
}

func TestExtractPaths(t *testing.T) {
	tests := []struct {
		command string
		want    []string
	}{
		{
			command: "cat .env",
			want:    []string{".env"},
		},
		{
			command: "cp .env.example .env",
			want:    []string{".env.example", ".env"},
		},
		{
			command: "git diff src/app.ts docs/readme.md",
			want:    []string{filepath.Clean("src/app.ts"), filepath.Clean("docs/readme.md")},
		},
		{
			command: "rm -rf ../outside/file.txt",
			want:    []string{filepath.Clean("../outside/file.txt")},
		},
		{
			command: "cat < /etc/shadow > ./out.txt",
			want:    []string{filepath.Clean("/etc/shadow"), filepath.Clean("./out.txt")},
		},
		{
			command: `pytest --config="pytest.ini" tests/test_api.py`,
			want:    []string{"pytest.ini", filepath.Clean("tests/test_api.py")},
		},
		{
			command: "git status",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := ExtractPaths(tt.command)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractPaths(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
