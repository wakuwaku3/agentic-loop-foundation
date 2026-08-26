package application

// V2-083: the caller-fault marker.
//
// internal/api's domainError answers 500 internal_error with a fixed message
// for any error it cannot classify, because an error nobody classified is by
// definition not known to be the caller's mistake and is not known to be safe
// to echo. That default is only correct if the errors that ARE the caller's
// mistake carry an identity the transport can test. Several of them did not:
// the validation refusals this repository authors with errors.New and
// fmt.Errorf -- "request_id is required", the page_size and export-limit
// bounds, "invalid control mode or scope" -- were 400 only because 400 was
// the default.
//
// ErrInvalidRequest is that identity, and invalidRequest is how a site claims
// it. The one property that makes this safe to apply to already-shipped
// refusals is that it changes NO MESSAGE: Error() delegates to the wrapped
// error, so "request_id is required" stays exactly "request_id is required" on
// the wire. That is why the marker is a small type rather than
// fmt.Errorf("%w: ...", ErrInvalidRequest), which would prepend this
// sentinel's own text to every one of those responses.
//
// Where it goes: at the site that AUTHORS a caller-input validation error.
// Never at a shared helper that also carries dependency failures, never in
// Service.transact or Service.mutate, and never around a transaction -- a
// marker that climbs that high drags storage faults back into 400 and
// recreates the defect behind a new spelling. errors_test.go asserts both
// halves: that every route-reachable validation refusal satisfies
// errors.Is(err, ErrInvalidRequest), and that a fault the request did not
// cause (a zero clock, a store decorator's error) does not.

import "errors"

// ErrInvalidRequest is the sentinel meaning "the request itself is wrong".
// internal/api maps it to 400 invalid_request. Its own text is never seen by
// a caller: every marked error answers with the message its authoring site
// wrote.
var ErrInvalidRequest = errors.New("invalid request")

// invalidRequestError marks one authored validation error. It is deliberately
// transparent in three directions at once: Error() returns the wrapped
// error's text unchanged, Unwrap() returns the wrapped error so any sentinel
// it already carries keeps its identity, and Is matches ErrInvalidRequest and
// nothing else -- an Is that matched more would move a 404 or a 409 to 400.
type invalidRequestError struct{ err error }

func (e invalidRequestError) Error() string { return e.err.Error() }

func (e invalidRequestError) Unwrap() error { return e.err }

func (e invalidRequestError) Is(target error) bool { return target == ErrInvalidRequest }

// invalidRequest marks err as a caller fault without touching its message. A
// nil error stays nil so a call site can wrap unconditionally.
func invalidRequest(err error) error {
	if err == nil {
		return nil
	}
	return invalidRequestError{err: err}
}
