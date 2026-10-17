package cli

// This file is the identity of the command line: the local principal that
// the verbs act as, and the verbs that manage agent profiles.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/bdubs00/featherweight-kernel/config"
	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

// localUser returns the name of the user that runs the process: the
// account name, else $USER, else "user".
func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return "user"
}

// localPrincipal is the principal of the user that runs the process. Its
// subject is local:<user> and its kind is local, so the access entries
// local:* and kind:local both name it. fw mcp and fw agents always act as
// it, and fw run, fw events, and fw replay act as it when access.default
// is closed or access.external is set.
func localPrincipal() *fw.Principal {
	name := localUser()
	return &fw.Principal{Subject: "local:" + name, Name: name, Kind: fw.KindLocal}
}

// accessClosed reports whether access.default is closed.
func accessClosed(cfg *fw.Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Access.Default), "closed")
}

// accessExternal reports whether access.external hands the decisions to a
// control plane. Then the control plane is the only source of truth, and
// the CLI asks it instead of reading access.admins.
func accessExternal(cfg *fw.Config) bool { return cfg.Access.External != nil }

// controlPlane names the decision point of access.external for a message,
// with the default tool filled in.
func controlPlane(cfg *fw.Config) string {
	if cfg.Access.External == nil {
		return "the control plane"
	}
	e := *cfg.Access.External
	if e.Tool == "" {
		e.Tool = fw.DefaultAuthzTool
	}
	return e.String()
}

// commandContext returns ctx with the local principal when access.default
// is closed or access.external is set, so the run carries the name of the
// operator and the access rules, or the control plane, decide for it. With
// the default open and no control plane, the context carries no
// principal, and the kernel trusts the call as one from the process
// itself.
func commandContext(ctx context.Context, cfg *fw.Config) context.Context {
	if accessClosed(cfg) || accessExternal(cfg) {
		return fw.WithPrincipal(ctx, localPrincipal())
	}
	return ctx
}

// isAdmin reports whether access.admins names the principal. It reads the
// local rules only. With access.external set, use adminOf.
func isAdmin(cfg *fw.Config, p *fw.Principal) bool {
	return fw.MatchesAny(cfg.Access.Admins, p)
}

// adminOf reports whether the principal has the admin role. With
// access.external set, it builds an agent and asks the control plane,
// because the control plane is then the only source of truth. Otherwise it
// reads access.admins.
func adminOf(ctx context.Context, cfg *fw.Config, p *fw.Principal) (bool, error) {
	if !accessExternal(cfg) {
		return isAdmin(cfg, p), nil
	}
	a, err := newAgent(ctx, cfg, modeManage)
	if err != nil {
		return false, err
	}
	defer closeAgent(a)
	err = a.Authorize(fw.WithPrincipal(ctx, p), fw.ActionAdmin, fw.Target{})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fw.ErrForbidden):
		return false, nil
	}
	return false, err
}

// cmdWhoami prints the local principal.
func cmdWhoami(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("whoami", "")
	asJSON := fs.Bool("json", false, "print the principal as JSON")
	describe(fs, `Prints the principal that fw mcp and fw agents act as, and that fw run,
fw events, and fw replay act as when access.default is closed or
access.external is set. Name it in access.admins as local:*, as kind:local,
or by its subject. With access.external, the control plane decides for it.`)
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	p := localPrincipal()
	if *asJSON {
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return g.fail("whoami", err)
		}
		fmt.Fprintf(g.stdout, "%s\n", b)
		return exitOK
	}
	fmt.Fprintf(g.stdout, "subject: %s\nname: %s\nkind: %s\n", p.Subject, p.Name, p.Kind)
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("whoami", err)
	}
	// The principal prints even when the control plane does not answer,
	// and the exit code reports the failure.
	admin, adminErr := adminOf(ctx, cfg, p)
	if adminErr != nil {
		fmt.Fprintln(g.stdout, "admin: unknown")
	} else {
		fmt.Fprintf(g.stdout, "admin: %t\n", admin)
	}
	switch {
	case accessExternal(cfg):
		fmt.Fprintf(g.stdout, "acts: always; access.external hands every decision to %s\n", controlPlane(cfg))
	case accessClosed(cfg):
		fmt.Fprintln(g.stdout, "acts: always; access.default is closed")
	default:
		fmt.Fprintln(g.stdout, "acts: in fw mcp and fw agents; access.default is open, so fw run is trusted")
	}
	if adminErr != nil {
		return g.fail("whoami", accessHint(cfg, adminErr))
	}
	return exitOK
}

// agentsDoc is the usage text of fw agents.
const agentsDoc = `Manages the agent profiles. The verb acts as the local principal (see fw
whoami): a profile that it creates names the principal as its owner, and a
change needs the owner role on the profile or an entry in access.admins
such as local:*.

  fw agents                 list the profiles with the model and the access block
  fw agents <name>          print one profile as YAML
  fw agents put <file.yaml> create or replace the profile that the file holds; the
                            name is the name field of the file, else the file name
  fw agents delete <name>   remove a profile

The kernel writes no profile: put and delete come with fw-workbench, which
writes <home>/agents/<name>.yaml. A running server reads the change at its
next start. A profile that fw.yaml defines is changed in fw.yaml, not here.
A put whose file has no access block keeps the access block of the profile
that it replaces. In the list, an access of - means that the profile
follows access.default.`

// cmdAgents lists, prints, puts, and deletes agent profiles.
func cmdAgents(ctx context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("agents", "[<name> | put <file.yaml> | delete <name>]")
	asJSON := fs.Bool("json", false, "print the profiles as JSON")
	name := fs.String("name", "", "put: the profile `name`, in place of the one in the file")
	describe(fs, agentsDoc)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return flagExit(err)
	}
	var sub, arg string
	switch {
	case len(pos) == 0:
	case len(pos) == 1 && pos[0] != "put" && pos[0] != "delete":
		sub, arg = "get", pos[0]
	case len(pos) == 2 && (pos[0] == "put" || pos[0] == "delete"):
		sub, arg = pos[0], pos[1]
	default:
		fs.Usage()
		return exitUsage
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("agents", err)
	}
	a, err := newAgent(ctx, cfg, modeManage)
	if err != nil {
		return g.fail("agents", err)
	}
	defer closeAgent(a)
	ctx = fw.WithPrincipal(ctx, localPrincipal())
	switch sub {
	case "get":
		return g.printProfile(ctx, a, arg, *asJSON)
	case "put":
		return g.putProfile(ctx, a, arg, *name)
	case "delete":
		if AgentsDelete == nil {
			return g.fail("agents", errNoAgentWrites)
		}
		if err := AgentsDelete(ctx, a, arg); err != nil {
			return g.fail("agents", accessHint(cfg, err))
		}
		fmt.Fprintf(g.stdout, "deleted the profile %s under %s; a running server reads the change at its next start\n", arg, filepath.Join(cfg.Home, "agents"))
		return exitOK
	}
	return g.listProfiles(ctx, a, *asJSON)
}

// listProfiles prints the profiles that the local principal can view.
func (g *globals) listProfiles(ctx context.Context, a *fw.Agent, asJSON bool) int {
	profiles, err := a.ListProfiles(ctx)
	if err != nil {
		return g.fail("agents", accessHint(a.Config(), err))
	}
	if asJSON {
		b, err := json.MarshalIndent(profiles, "", "  ")
		if err != nil {
			return g.fail("agents", err)
		}
		fmt.Fprintf(g.stdout, "%s\n", b)
		return exitOK
	}
	tw := tabwriter.NewWriter(g.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDESCRIPTION\tMODEL\tACCESS")
	for _, p := range profiles {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, firstNonEmpty(firstLine(p.Description), "-"), firstNonEmpty(p.Model, "-"), accessLine(p.Access))
	}
	if err := tw.Flush(); err != nil {
		return g.fail("agents", err)
	}
	return exitOK
}

// accessLine words an access block on one line, or "-" for none.
func accessLine(acc *fw.Access) string {
	if acc == nil {
		return "-"
	}
	var parts []string
	for _, l := range []struct {
		role    string
		entries []string
	}{{"owners", acc.Owners}, {"operators", acc.Operators}, {"viewers", acc.Viewers}} {
		if len(l.entries) > 0 {
			parts = append(parts, l.role+"="+strings.Join(l.entries, ","))
		}
	}
	if len(parts) == 0 {
		return "(nobody)"
	}
	return strings.Join(parts, " ")
}

// printProfile prints one profile as YAML or JSON.
func (g *globals) printProfile(ctx context.Context, a *fw.Agent, name string, asJSON bool) int {
	p, ok := a.ProfileNamed(name)
	if !ok {
		// The list holds the built-in profile "default" when no profile is
		// configured, so the command prints what the list names.
		visible, err := a.ListProfiles(ctx)
		if err != nil {
			return g.fail("agents", accessHint(a.Config(), err))
		}
		var names []string
		for _, q := range visible {
			if q.Name == name {
				p, ok = q, true
			}
			names = append(names, q.Name)
		}
		if !ok {
			return g.fail("agents", fmt.Errorf("no profile is named %q; the profiles are %s", name, strings.Join(names, ", ")))
		}
	}
	if err := a.Authorize(ctx, fw.ActionAgentView, fw.Target{Agent: name}); err != nil {
		return g.fail("agents", accessHint(a.Config(), err))
	}
	var b []byte
	var err error
	if asJSON {
		b, err = json.MarshalIndent(p, "", "  ")
	} else {
		b, err = yaml.Marshal(p)
	}
	if err != nil {
		return g.fail("agents", err)
	}
	fmt.Fprintf(g.stdout, "%s\n", strings.TrimRight(string(b), "\n"))
	return exitOK
}

// errNoAgentWrites is the answer of put and delete in a binary without a
// package that manages profiles.
var errNoAgentWrites = errors.New("fw writes no agent profile; fw-workbench agents put and delete do, or write the file under <home>/agents yourself")

// putProfile reads a profile file and creates or replaces the profile.
func (g *globals) putProfile(ctx context.Context, a *fw.Agent, path, name string) int {
	if AgentsPut == nil {
		return g.fail("agents", errNoAgentWrites)
	}
	p, err := config.LoadProfile(path)
	if err != nil {
		return g.fail("agents", err)
	}
	if name != "" {
		p.Name = name
	}
	_, existed := a.ProfileNamed(p.Name)
	if err := AgentsPut(ctx, a, p); err != nil {
		return g.fail("agents", accessHint(a.Config(), err))
	}
	verb := "created"
	if existed {
		verb = "replaced"
	}
	fmt.Fprintf(g.stdout, "%s the profile %s under %s; a running server reads the change at its next start\n", verb, p.Name, filepath.Join(a.Config().Home, "agents"))
	return exitOK
}

// accessHint adds what to do to a forbidden error, and to an error of a
// control plane that did not answer.
func accessHint(cfg *fw.Config, err error) error {
	switch {
	case errors.Is(err, fw.ErrAuthzUnavailable):
		return fmt.Errorf("%w (the control plane did not answer: make sure that %s is reachable; fw doctor checks it)", err, controlPlane(cfg))
	case errors.Is(err, fw.ErrForbidden) && accessExternal(cfg):
		return fmt.Errorf("%w (%s refused it; access.admins and the access blocks do not apply while access.external is set)", err, controlPlane(cfg))
	}
	if errors.Is(err, fw.ErrForbidden) {
		return fmt.Errorf("%w (add local:* to access.admins in fw.yaml, or name the principal of fw whoami in the access block of the profile)", err)
	}
	return err
}
