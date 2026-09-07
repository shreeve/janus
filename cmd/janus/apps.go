package main

// What is registered with the running edge: the hot registry the control
// API keeps, read the way status counts it, printed one app per line.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func appsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "apps [--json]",
		Long: `
Lists what is registered with the running edge over its control API: each
app's name, the hosts it answers for, what serves them (worker sockets, a
files root, or a site directory), and its lease. --json prints the
registry as the control API returns it (GET /1.0/apps).

Exit 3 when no edge answers.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			p, note := delegatedPaths()
			body, at, err := controlGet(p, "/1.0/apps")
			if err != nil {
				cmd.SilenceErrors = true
				fmt.Fprintf(cmd.ErrOrStderr(), "janus is not running: no control plane answered at %s or %s\n", p.sock, localControlURL)
				return &exitError{code: 3, err: err}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				_, err := out.Write(append(body, '\n'))
				return err
			}
			if note != "" {
				fmt.Fprintln(out, note)
			}
			var apps []registeredApp
			if err := json.Unmarshal(body, &apps); err != nil {
				return fmt.Errorf("the control API's app list does not parse: %v", err)
			}
			if len(apps) == 0 {
				fmt.Fprintf(out, "no apps registered (control %s)\n", at)
				return nil
			}
			fmt.Fprintf(out, "%d app%s registered (control %s)\n", len(apps), plural(len(apps)), at)
			tw := tabwriter.NewWriter(out, 2, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tHOSTS\tSERVES\tLEASE\tID")
			for _, a := range apps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Name, strings.Join(a.Hosts, " "), a.serves(), a.Lease, a.ID)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "Print the registry as JSON")
	return cmd
}

// registeredApp is the part of a control API app record the listing shows.
type registeredApp struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hosts     []string `json:"hosts"`
	Lease     string   `json:"lease"`
	Upstreams []struct {
		Path     string `json:"path"`
		Doorbell bool   `json:"doorbell"`
	} `json:"upstreams"`
	Site *struct {
		Host string `json:"host"`
		Dir  string `json:"dir"`
	} `json:"site"`
	Files *struct {
		Roots []struct {
			Path   string `json:"path"`
			Browse bool   `json:"browse"`
		} `json:"roots"`
	} `json:"files"`
}

// serves says what answers for the app's hosts.
func (a registeredApp) serves() string {
	var parts []string
	switch {
	case len(a.Upstreams) == 1 && a.Upstreams[0].Doorbell:
		parts = append(parts, "doorbell "+a.Upstreams[0].Path)
	case len(a.Upstreams) > 0:
		parts = append(parts, fmt.Sprintf("%d worker%s", len(a.Upstreams), plural(len(a.Upstreams))))
	}
	if a.Site != nil {
		parts = append(parts, "sites in "+a.Site.Dir)
	}
	if a.Files != nil {
		for _, r := range a.Files.Roots {
			kind := "files"
			if r.Browse {
				kind = "browse"
			}
			parts = append(parts, kind+" "+r.Path)
		}
	}
	if len(parts) == 0 {
		return "nothing yet"
	}
	return strings.Join(parts, ", ")
}

// controlGet reads a control API path from this edge: over its unix
// socket, else over loopback when the edge there proves it is this one
// (its control list names the socket). Returns the body and where it
// answered.
func controlGet(p servicePaths, path string) ([]byte, string, error) {
	if socketExists(p.sock) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.sock)
		}
		if body, err := controlBody(controlClient(dial), "http://janus"+path); err == nil {
			return body, "unix " + p.sock, nil
		}
	}
	if !controlListensOn(localControlURL+"/1.0", p.sock) {
		return nil, "", errors.New("no control plane answered")
	}
	body, err := controlBody(controlClient(nil), localControlURL+path)
	if err != nil {
		return nil, "", err
	}
	return body, localControlURL, nil
}

func controlBody(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
