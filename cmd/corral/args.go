package main

import "strings"

// interspersedFlagArgs makes the documented "command <id> --flag" form work
// with Go's standard flag package, which otherwise stops parsing at the first
// positional argument. valueFlags names flags whose following token belongs to
// the flag; all other flag-looking tokens are treated as booleans and moved on
// their own. The returned order is flags first, positionals last, preserving
// the relative order within both groups.
func interspersedFlagArgs(args []string, valueFlags ...string) []string {
	values := make(map[string]bool, len(valueFlags))
	for _, name := range valueFlags {
		values[name] = true
	}

	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}

		flags = append(flags, arg)
		flagToken := strings.SplitN(arg, "=", 2)
		name := strings.TrimLeft(flagToken[0], "-")
		if values[name] && len(flagToken) == 1 && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positionals...)
}
