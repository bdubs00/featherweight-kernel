package fw

import "context"

// PrincipalOfRun returns the principal that started a run, as the run info
// holds it: the subject and the kind. A run without a principal, such as a
// local run or a job that a publisher sent without one, gives nil. The
// principal has no name, no groups, and no scopes, because the ledger
// records the subject and the kind alone, so a rule that matches a group
// does not match it.
func PrincipalOfRun(info RunInfo) *Principal {
	if info.Principal == "" {
		return nil
	}
	return &Principal{Subject: info.Principal, Kind: info.PrincipalKind}
}

// WithRunPrincipal returns ctx with the principal of a run in it, the way
// a surface puts the principal of a request there (WithPrincipal). The
// kernel then authorizes each call on the context, such as List and Get,
// as a call of that principal. A tool that reads runs for the run that
// called it uses it, so the run reads no more than the person who started
// it can view. A run without a principal leaves ctx as it is: the call is
// then a call of the process itself.
func WithRunPrincipal(ctx context.Context, info RunInfo) context.Context {
	p := PrincipalOfRun(info)
	if p == nil {
		return ctx
	}
	return WithPrincipal(ctx, p)
}
