package swe

// MissingEnvError is returned during construction when a required environment
// variable is unset. In P1 the only required variable is LLM_API_KEY, and only
// when the selected model's provider requires a key. It is errors.As-recoverable
// so the composition root can report which variable was missing.
type MissingEnvError struct{ Var string }

func (e *MissingEnvError) Error() string {
	return "swe: required environment variable " + e.Var + " is not set"
}

// WorkspaceRootError is returned during construction when the workspace root (the
// process working directory) cannot be resolved, so the file tools cannot be
// confined to a known root. Cause carries the underlying os.Getwd error.
type WorkspaceRootError struct{ Cause error }

func (e *WorkspaceRootError) Error() string {
	if e.Cause == nil {
		return "swe: cannot resolve workspace root"
	}
	return "swe: cannot resolve workspace root: " + e.Cause.Error()
}

func (e *WorkspaceRootError) Unwrap() error { return e.Cause }

// PrimaryToolSetError is returned during construction when the primary operator's
// fail-secure PermissionChecker cannot be built — currently only when $HOME is unresolvable
// while a home-relative ("~/…") deny pattern is configured. It wraps the underlying cause
// (e.g. *tools.HomeUnresolvableError) so a caller can errors.As it, and it exists so the
// primary operator's tool set never falls OPEN (a nil, checker-less tool set) on that error.
type PrimaryToolSetError struct{ Cause error }

func (e *PrimaryToolSetError) Error() string {
	if e.Cause == nil {
		return "swe: cannot build primary operator tool set"
	}
	return "swe: cannot build primary operator tool set: " + e.Cause.Error()
}

func (e *PrimaryToolSetError) Unwrap() error { return e.Cause }
