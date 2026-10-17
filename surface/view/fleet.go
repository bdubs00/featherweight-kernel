package view

// This file holds the fleet parts of the view model (docs/ARCHITECTURE.md
// section 8.6): the runs of the whole fleet, the child runs of a run that
// another member owns, and the members as text. Every function works
// without the fleet too, and then it covers this process only.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Scopes of a run listing. The fleet scope covers every member, and the
// local scope covers this process only.
const (
	ScopeFleet = "fleet"
	ScopeLocal = "local"
)

// Runs lists the runs that the caller in ctx can view, newest first. With
// the fleet on and the scope fleet, the default, the list holds the runs of
// the other members too, each with its member. Member narrows the list to
// one member, and the name of this process selects its own runs. It returns
// an error that wraps fw.ErrInvalidRequest for an unknown scope, and for a
// member when the fleet is off.
func Runs(ctx context.Context, a *fw.Agent, scope, member string) ([]*fw.Run, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	switch scope {
	case "", ScopeFleet, ScopeLocal:
	default:
		return nil, fmt.Errorf("%w: scope %q is not %s or %s", fw.ErrInvalidRequest, scope, ScopeFleet, ScopeLocal)
	}
	member = strings.TrimSpace(member)
	f := a.Fleet()
	if f == nil {
		if member != "" {
			return nil, fmt.Errorf("%w: the fleet is off, so no member has the name %q; set fleet.subject to turn it on", fw.ErrInvalidRequest, member)
		}
		return a.List(ctx)
	}
	self := f.Name()
	var out []*fw.Run
	if member == "" || member == self {
		local, err := a.List(ctx)
		if err != nil {
			return nil, err
		}
		out = local
	}
	if scope != ScopeLocal && member != self {
		remote, err := f.Runs(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range remote {
			if member == "" || r.Member == member {
				out = append(out, r)
			}
		}
		SortRuns(out)
	}
	return out, nil
}

// Children lists the child runs of a run that the caller can view, newest
// first. The parent can belong to another member of the fleet. A parent
// that nobody knows gives an error that wraps fw.ErrRunNotFound.
func Children(ctx context.Context, a *fw.Agent, parent string) ([]*fw.Run, error) {
	kids, err := a.Children(ctx, parent)
	f := a.Fleet()
	if !errors.Is(err, fw.ErrRunNotFound) || f == nil {
		return kids, err
	}
	if _, ferr := f.Run(ctx, parent); ferr != nil {
		if errors.Is(ferr, fw.ErrRunNotFound) {
			return nil, err
		}
		return nil, ferr
	}
	remote, ferr := f.Runs(ctx)
	if ferr != nil {
		return nil, ferr
	}
	out := []*fw.Run{}
	for _, r := range remote {
		if r.Parent == parent {
			out = append(out, r)
		}
	}
	SortRuns(out)
	return out, nil
}

// SortRuns sorts runs newest first, as the kernel lists them.
func SortRuns(runs []*fw.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.After(runs[j].CreatedAt)
		}
		return runs[i].ID > runs[j].ID
	})
}
