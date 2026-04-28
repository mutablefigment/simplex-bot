package bot

import "strings"

type Cmd struct {
	Name string
	Args string
}

func parseCommand(text string) (Cmd, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return Cmd{}, false
	}
	rest := strings.TrimPrefix(t, "/")
	if rest == "" {
		return Cmd{}, false
	}
	name, args, _ := strings.Cut(rest, " ")
	return Cmd{Name: name, Args: strings.TrimSpace(args)}, true
}
