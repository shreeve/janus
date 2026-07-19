package janus

// resolveBool applies cascade: site override → global default → built-in default.
func resolveBool(site, global *bool, builtin bool) bool {
	if site != nil {
		return *site
	}
	if global != nil {
		return *global
	}
	return builtin
}

// parseOnOff parses trailing args for a boolean capability.
//
//	ping           → true
//	ping on        → true
//	ping off       → false
func parseOnOff(args []string) (bool, error) {
	switch len(args) {
	case 0:
		return true, nil
	case 1:
		switch args[0] {
		case "on":
			return true, nil
		case "off":
			return false, nil
		default:
			return false, errInvalidOnOff(args[0])
		}
	default:
		return false, errTooManyOnOffArgs()
	}
}

type onOffError string

func (e onOffError) Error() string { return string(e) }

func errInvalidOnOff(v string) error {
	return onOffError(`want "on" or "off", got "` + v + `"`)
}

func errTooManyOnOffArgs() error {
	return onOffError(`want at most one of "on" or "off"`)
}
