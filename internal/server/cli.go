package server

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/config"
)

func init() {
	app.RegisterServeHandler(RunServe)
}

// RunServe parses arguments and launches the Relayer Web Gateway.
func RunServe(arguments []string, _ io.Writer, diagnostics io.Writer) error {
	if diagnostics == nil {
		diagnostics = io.Discard
	}

	flags := flag.NewFlagSet("relayer serve", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(diagnostics, "Usage: relayer serve [--bind address] [--port port] [--token secret] [--viewer-token secret] [--config path] [--static-dir dir]")
		flags.PrintDefaults()
	}

	bind := flags.String("bind", "127.0.0.1", "Network interface address to bind (e.g., 127.0.0.1 or 0.0.0.0)")
	port := flags.Int("port", 8080, "Port to listen on")
	token := flags.String("token", "", "Operator authentication token secret (auto-generated if empty when binding outside localhost)")
	operatorToken := flags.String("operator-token", "", "Operator authentication token (alias for --token)")
	viewerToken := flags.String("viewer-token", "", "Viewer (read-only) authentication token secret")
	configPath := flags.String("config", config.DefaultPath, "Path to YAML configuration file")
	staticDir := flags.String("static-dir", "", "Custom filesystem directory for static web UI assets (default uses embedded assets)")

	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("relayer serve accepts no positional arguments")
	}

	ctx, cancel := serverSignalContext()
	defer cancel()

	finalOperatorToken := strings.TrimSpace(*token)
	if finalOperatorToken == "" {
		finalOperatorToken = strings.TrimSpace(*operatorToken)
	} else if strings.TrimSpace(*operatorToken) != "" {
		finalOperatorToken += "," + strings.TrimSpace(*operatorToken)
	}

	opts := Options{
		Bind:        strings.TrimSpace(*bind),
		Port:        *port,
		Token:       finalOperatorToken,
		ViewerToken: strings.TrimSpace(*viewerToken),
		ConfigPath:  strings.TrimSpace(*configPath),
		StaticDir:   strings.TrimSpace(*staticDir),
		Diagnostics: diagnostics,
	}

	return Serve(ctx, opts)
}
