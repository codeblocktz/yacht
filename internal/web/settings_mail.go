package web

import "github.com/codeblocktz/yacht/internal/mailstatus"

// mailFailureTone colours the last failure by whether mail has worked since:
// a failure followed by a success is history, one with nothing after it is
// what is happening now.
func mailFailureTone(st *mailstatus.Status) string {
	if st.LastSent.After(st.LastFailure) {
		return "status-warn"
	}
	return "status-err"
}
