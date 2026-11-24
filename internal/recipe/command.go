package recipe

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseCommand parses one verb-first command string:
//
//	run [--no-cache] [--mount=type=cache,target=/path[,id=ID][,sharing=shared|private|locked]]...
//	    [--mount=type=tmpfs,target=/path]... [--mount=type=bind,from=NAME,target=/path[,source=/path]]...
//	    <shell script>
//	env NAME=VALUE
//	copy [--from=STAGE] [--chmod=0644] [--chown=UID:GID] <src>... <dst>
//
// For `run`, everything after the flags is passed verbatim to `/bin/sh -c`
// (whitespace preserved). For `copy`, the remainder is whitespace-split;
// quoting is not supported in v0 (paths with spaces are a known limitation).
func ParseCommand(raw string) (Command, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, errf("empty command")
	}
	verb, rest := splitWord(s)
	switch verb {
	case "run":
		return parseRun(raw, rest)
	case "copy":
		return parseCopy(raw, rest)
	case "env":
		return parseEnv(raw, rest)
	default:
		return nil, errf("unknown verb %q (expected \"run\", \"copy\" or \"env\")", verb)
	}
}

func parseRun(raw, rest string) (Command, error) {
	cmd := RunCmd{raw: raw}
	for {
		rest = strings.TrimLeft(rest, " \t")
		if !strings.HasPrefix(rest, "--") {
			break
		}
		var flag string
		flag, rest = splitWord(rest)
		switch {
		case flag == "--no-cache":
			cmd.NoCache = true
		case strings.HasPrefix(flag, "--mount="):
			m, err := parseMount(strings.TrimPrefix(flag, "--mount="))
			if err != nil {
				return nil, err
			}
			cmd.Mounts = append(cmd.Mounts, m)
		default:
			return nil, errf("run: unknown flag %q", flag)
		}
	}
	script := strings.TrimSpace(rest)
	if script == "" {
		return nil, errf("run: missing shell script")
	}
	cmd.Script = script
	return cmd, nil
}

// parseEnv parses `env NAME=VALUE`. Everything after the first "=" is the
// value, so values may contain spaces; one variable per command.
func parseEnv(raw, rest string) (Command, error) {
	name, value, ok := strings.Cut(strings.TrimSpace(rest), "=")
	if !ok || !validEnvName(name) {
		return nil, errf("env: expected NAME=VALUE")
	}
	return EnvCmd{Name: name, Value: value, raw: raw}, nil
}

func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func parseMount(spec string) (Mount, error) {
	m := Mount{}
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return m, errf("run: malformed --mount option %q", kv)
		}
		switch k {
		case "type":
			if v != "cache" && v != "tmpfs" && v != "bind" {
				return m, errf("run: --mount type %q not supported (\"cache\", \"tmpfs\" or \"bind\")", v)
			}
			m.Type = v
		case "target", "dst", "destination":
			m.Target = v
		case "id":
			m.ID = v
		case "from":
			m.From = v
		case "source", "src":
			m.Source = v
		case "sharing":
			if v != "shared" && v != "private" && v != "locked" {
				return m, errf("run: --mount sharing %q invalid (shared|private|locked)", v)
			}
			m.Sharing = v
		default:
			return m, errf("run: unknown --mount option %q", k)
		}
	}
	if m.Type != "bind" && (m.From != "" || m.Source != "") {
		return m, errf("run: from= and source= apply to type=bind only")
	}
	switch m.Type {
	case "":
		return m, errf("run: --mount requires type=cache, type=tmpfs or type=bind")
	case "tmpfs", "bind":
		if m.ID != "" || m.Sharing != "" {
			return m, errf("run: id= and sharing= apply to type=cache only")
		}
		if m.Type == "bind" {
			if m.From == "" {
				return m, errf("run: --mount=type=bind requires from= (a prepare stage or an earlier phase)")
			}
			if m.Source == "" {
				m.Source = "/"
			}
			if !strings.HasPrefix(m.Source, "/") {
				return m, errf("run: --mount source= must be an absolute path")
			}
		}
	case "cache":
		if m.ID == "" {
			m.ID = m.Target
		}
		if m.Sharing == "" {
			m.Sharing = "shared"
		}
	}
	if m.Target == "" || !strings.HasPrefix(m.Target, "/") {
		return m, errf("run: --mount requires an absolute target= path")
	}
	return m, nil
}

func parseCopy(raw, rest string) (Command, error) {
	cmd := CopyCmd{raw: raw}
	fields := strings.Fields(rest)
	i := 0
	for ; i < len(fields) && strings.HasPrefix(fields[i], "--"); i++ {
		f := fields[i]
		switch {
		case strings.HasPrefix(f, "--from="):
			cmd.From = strings.TrimPrefix(f, "--from=")
			if cmd.From == "" {
				return nil, errf("copy: --from requires a stage name")
			}
		case strings.HasPrefix(f, "--chmod="):
			v := strings.TrimPrefix(f, "--chmod=")
			n, err := strconv.ParseUint(v, 8, 32)
			if err != nil {
				return nil, errf("copy: --chmod=%q is not an octal mode", v)
			}
			mode := uint32(n)
			cmd.Chmod = &mode
		case strings.HasPrefix(f, "--chown="):
			v := strings.TrimPrefix(f, "--chown=")
			uid, gid, ok := strings.Cut(v, ":")
			if !ok || atoiOK(uid) < 0 || atoiOK(gid) < 0 {
				return nil, errf("copy: --chown=%q must be numeric UID:GID (name lookup not supported yet)", v)
			}
			cmd.Chown = v
		default:
			return nil, errf("copy: unknown flag %q", f)
		}
	}
	paths := fields[i:]
	if len(paths) < 2 {
		return nil, errf("copy: need at least <src> <dst>")
	}
	cmd.Srcs = paths[:len(paths)-1]
	cmd.Dst = paths[len(paths)-1]
	if !strings.HasPrefix(cmd.Dst, "/") {
		return nil, errf("copy: destination %q must be absolute", cmd.Dst)
	}
	for _, s := range cmd.Srcs {
		if cmd.From == "" && strings.HasPrefix(s, "/") {
			return nil, errf("copy: context source %q must be relative (absolute sources only with --from)", s)
		}
	}
	return cmd, nil
}

func splitWord(s string) (word, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t\n")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

func atoiOK(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

var _ = fmt.Sprintf // keep fmt import stable if errf moves
