// testkit is the acceptance-suite support binary for test.sh: fixture
// HTTP servers on unix sockets, an RFC 6455 WebSocket driver, and small
// shell utilities. It is a test instrument, never shipped.
//
//	testkit upstream  --sock S --name N --hits F
//	testkit doorbell  --sock S --app ID --newsock S2 --ring F
//	testkit cacheup   --sock S --hits F
//	testkit hubtenant --sock S --hits F --playbook F ID…
//	testkit wedge     --host H
//	testkit ws        HOST ORIGIN|- COOKIE|- CMD…
//	testkit json get KEY        (stdin JSON object → field value)
//	testkit upstreams HOST      (stdin /1.0/apps JSON → that app's upstreams)
//	testkit repeat STR N
//	testkit now-ns
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		die("usage: testkit <subcommand> …")
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "upstream":
		cmdUpstream(args)
	case "doorbell":
		cmdDoorbell(args)
	case "cacheup":
		cmdCacheUpstream(args)
	case "hubtenant":
		cmdHubTenant(args)
	case "wedge":
		cmdWedge(args)
	case "ws":
		cmdWS(args)
	case "json":
		cmdJSON(args)
	case "upstreams":
		cmdUpstreams(args)
	case "repeat":
		cmdRepeat(args)
	case "now-ns":
		fmt.Println(time.Now().UnixNano())
	default:
		die("testkit: unknown subcommand %q", cmd)
	}
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

// cmdJSON: `testkit json get KEY` — print one field of the JSON object on
// stdin. Strings print raw, numbers verbatim; a missing key is an error.
func cmdJSON(args []string) {
	if len(args) != 2 || args[0] != "get" {
		die("usage: testkit json get KEY")
	}
	key := args[1]
	dec := json.NewDecoder(os.Stdin)
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		die("testkit json: %v", err)
	}
	v, ok := obj[key]
	if !ok {
		die("testkit json: no key %q", key)
	}
	switch t := v.(type) {
	case string:
		fmt.Println(t)
	case json.Number:
		fmt.Println(t.String())
	case bool:
		fmt.Println(t)
	case nil:
		fmt.Println("null")
	default:
		out, _ := json.Marshal(t)
		fmt.Println(string(out))
	}
}

// cmdUpstreams: `testkit upstreams HOST` — from the /1.0/apps JSON on
// stdin, print the upstream list (verbatim bytes) of the app that claims
// HOST. No match prints nothing.
func cmdUpstreams(args []string) {
	if len(args) != 1 {
		die("usage: testkit upstreams HOST")
	}
	host := args[0]
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		die("testkit upstreams: %v", err)
	}
	var apps []struct {
		Hosts     []string        `json:"hosts"`
		Upstreams json.RawMessage `json:"upstreams"`
	}
	if err := json.Unmarshal(data, &apps); err != nil {
		die("testkit upstreams: %v", err)
	}
	for _, a := range apps {
		for _, h := range a.Hosts {
			if h == host {
				fmt.Println(string(a.Upstreams))
				return
			}
		}
	}
}

// cmdRepeat: `testkit repeat STR N` — print STR repeated N times.
func cmdRepeat(args []string) {
	if len(args) != 2 {
		die("usage: testkit repeat STR N")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 0 {
		die("testkit repeat: bad count %q", args[1])
	}
	fmt.Println(strings.Repeat(args[0], n))
}
