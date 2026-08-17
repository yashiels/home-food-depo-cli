package main

// STUB — Agent B implements all handlers + help against the contract in types.go.
func cmdMenu(d *Deps, a []string) (interface{}, *CLIError)   { return nil, notImpl("menu") }
func cmdMenus(d *Deps, a []string) (interface{}, *CLIError)  { return nil, notImpl("menus") }
func cmdOrder(d *Deps, a []string) (interface{}, *CLIError)  { return nil, notImpl("order") }
func cmdOrders(d *Deps, a []string) (interface{}, *CLIError) { return nil, notImpl("orders") }
func cmdCancel(d *Deps, a []string) (interface{}, *CLIError) { return nil, notImpl("cancel") }
func cmdCall(d *Deps, a []string) (interface{}, *CLIError)   { return nil, notImpl("call") }
func cmdGet(d *Deps, a []string) (interface{}, *CLIError)    { return nil, notImpl("get") }
func cmdNext(d *Deps, a []string) (interface{}, *CLIError)   { return nil, notImpl("next") }

func notImpl(c string) *CLIError { return &CLIError{Code: CodeUsage, Message: c + " not implemented"} }

func helpText() string              { return "hfd — Home Food Deli CLI (stub)\n" }
func cmdHelpText(cmd string) string { return "hfd " + cmd + " (stub)\n" }
func helpJSON() interface{}         { return map[string]any{"version": contractVersion, "commands": []any{}} }
