package fw

// This file holds the person for whom a trusted caller acts
// (docs/ARCHITECTURE.md section 8.2). The front of a fleet decides the
// action of a person on a run of a worker, then relays it with the static
// token and names the person. The records of the action name that person,
// and the kernel still authorizes the caller alone.

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxActingForBytes bounds the subject of WithActingFor. It is the bound of
// the person of a reaction, so the person can react too.
const MaxActingForBytes = 256

type actingForKey struct{}

// actingFor is the person that WithActingFor names, with the caller that
// named it.
type actingFor struct {
	caller *Principal
	person string
}

// WithActingFor returns ctx with the person for whom the caller in ctx
// acts, such as the front of a fleet that relays the decision of a person
// with the static token. The records of the action then name that person
// in place of the caller: the decider of an approval, the sender of a
// follow-up (MessageReceivedData.By), the person of a change of a budget,
// of a cancel, and of a reaction, and the person of a branch. The
// authorization does not change. PrincipalFrom still returns the caller,
// and the kernel decides for the caller alone, so the caller decides for
// the person before it acts.
//
// Only the process itself, the static token, or a workload acts for
// another person, as for WithMessageFrom. Any other principal gets
// ErrForbidden. An empty subject and the subject of the caller return ctx
// as it is. A subject of more than MaxActingForBytes bytes, with a control
// character, or not UTF-8 is ErrInvalidRequest. The person holds only
// while the principal in ctx is the caller that named it.
func WithActingFor(ctx context.Context, subject string) (context.Context, error) {
	subject = strings.TrimSpace(subject)
	p := PrincipalFrom(ctx)
	switch {
	case subject == "":
		return ctx, nil
	case len(subject) > MaxActingForBytes || strings.IndexFunc(subject, unicode.IsControl) >= 0 || !utf8.ValidString(subject):
		return ctx, fmt.Errorf("%w: the person to act for holds more than %d bytes or a control character, or is not UTF-8", ErrInvalidRequest, MaxActingForBytes)
	case p != nil && subject == p.Subject:
		return ctx, nil
	case !speaksForOthers(ctx):
		return ctx, fmt.Errorf("%w: only the static token or a workload can act for another person", ErrForbidden)
	}
	return context.WithValue(ctx, actingForKey{}, actingFor{caller: p, person: subject}), nil
}

// ActingFor returns the person for whom the caller in ctx acts
// (WithActingFor), or "" when the caller acts for itself.
func ActingFor(ctx context.Context) string {
	v, ok := ctx.Value(actingForKey{}).(actingFor)
	if !ok || v.caller != PrincipalFrom(ctx) || !speaksForOthers(ctx) {
		return ""
	}
	return v.person
}

// personOf returns the subject that a record of an action of the caller in
// ctx names: the person for whom the caller acts (ActingFor), else the
// subject of the principal, or "" for the process itself. The records read
// it, and no decision of the authorization does.
func personOf(ctx context.Context) string {
	if person := ActingFor(ctx); person != "" {
		return person
	}
	if p := PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
}
