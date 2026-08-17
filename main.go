package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// handler returns command data (rendered as ok-envelope) or a *CLIError.
// Handlers MUST NOT print or call os.Exit — they return up to main.
type handler func(d *Deps, args []string) (interface{}, *CLIError)

var handlers = map[string]handler{
	"menu":   cmdMenu,
	"menus":  cmdMenus,
	"order":  cmdOrder,
	"orders": cmdOrders,
	"cancel": cmdCancel,
	"call":   cmdCall,
	"get":    cmdGet,
	"next":   cmdNext,
}

func main() {
	args := os.Args[1:]

	// Help is plain TEXT (not the JSON envelope). No-args → help, exit 0.
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) >= 2 && args[1] == "--json" {
			b, _ := json.MarshalIndent(helpJSON(), "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Print(helpText())
		}
		return
	}

	cmd, rest := args[0], args[1:]
	// `hfd <cmd> --help` → plain text for that command.
	if len(rest) > 0 && (rest[0] == "--help" || rest[0] == "-h") {
		fmt.Print(cmdHelpText(cmd))
		return
	}

	h, ok := handlers[cmd]
	if !ok {
		emit(nil, &CLIError{Code: CodeUsage, Message: "unknown command: " + cmd})
		return
	}

	d, cliErr := buildDeps(tokenRequired[cmd])
	if cliErr != nil {
		emit(nil, cliErr)
		return
	}

	data, err := h(d, rest)
	emit(data, err)
}

// tokenRequired: only edge-function commands need the personal token. menu/menus/get use the
// anon key; next is pure local date math — none of them should fail when no token is configured.
var tokenRequired = map[string]bool{
	"order":  true,
	"cancel": true,
	"orders": true,
	"call":   true,
}

// emit renders exactly one JSON document and sets the exit code.
func emit(data interface{}, cliErr *CLIError) {
	var env Envelope
	code := 0
	if cliErr != nil {
		env = cliErr.toEnvelope()
		code = cliErr.exitCode()
	} else {
		env = okEnvelope(data)
	}
	b, mErr := json.MarshalIndent(env, "", "  ")
	if mErr != nil {
		// Last-resort: never leak internals; emit a minimal REMOTE error.
		fmt.Println(`{"version":1,"ok":false,"error":{"code":"REMOTE","message":"failed to encode result","retryable":false}}`)
		os.Exit(2)
	}
	fmt.Println(string(b))
	os.Exit(code)
}

// buildDeps loads the personal token (edge-function auth) and wires dependencies.
// Token source: env HFD_TOKEN, else ~/.config/hfd/token (must be 0600 or stricter).
// The token is never printed, logged, or placed in argv.
func buildDeps(needToken bool) (*Deps, *CLIError) {
	var tok string
	if needToken {
		var cliErr *CLIError
		tok, cliErr = loadToken()
		if cliErr != nil {
			return nil, cliErr
		}
	}
	return &Deps{
		Backend: newHTTPBackend(tok),
		Clock:   realClock{},
		Token:   tok,
		Stdin:   os.Stdin,
	}, nil
}

func loadToken() (string, *CLIError) {
	if t := strings.TrimSpace(os.Getenv("HFD_TOKEN")); t != "" {
		return t, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &CLIError{Code: CodeAuth, Message: "no HFD_TOKEN and cannot resolve home dir"}
	}
	p := filepath.Join(home, ".config", "hfd", "token")
	info, err := os.Stat(p)
	if err != nil {
		return "", &CLIError{Code: CodeAuth, Message: "no token: set HFD_TOKEN or write ~/.config/hfd/token (chmod 600)"}
	}
	// Refuse world/group-accessible token files.
	if info.Mode().Perm()&0o077 != 0 {
		return "", &CLIError{Code: CodeAuth, Message: fmt.Sprintf("token file %s has permissions %o; must be 0600", p, info.Mode().Perm())}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", &CLIError{Code: CodeAuth, Message: "cannot read token file"}
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", &CLIError{Code: CodeAuth, Message: "token file is empty"}
	}
	return t, nil
}
