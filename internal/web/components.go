package web

import "time"

// Shared display types.
//
// These exist because three different surfaces — a domain proving itself, a
// build running, a node joining — are all the same shape: a sequence of stages
// where one is current, the earlier ones are settled, and the interesting
// information is what the current one last saw. Modelling that once means the
// three cannot drift into three different vocabularies for the same idea.

// StepState is how far a stage has got. The values are the CSS class names
// directly, declared in assets/css/input.css under @layer components — a name
// assembled in Go is invisible to Tailwind's scanner, so anything returned from
// here has to exist in the stylesheet already.
type StepState string

const (
	// StepWait is a stage nothing has reached. Drawn, but making no claim.
	StepWait StepState = "step-wait"
	// StepActive is the stage being worked on now.
	StepActive StepState = "step-active"
	// StepDone is settled and behind us.
	StepDone StepState = "step-done"
	// StepErr is settled and wrong, which is different from still waiting.
	StepErr StepState = "step-err"
)

// Step is one stage of something that arrives over time.
type Step struct {
	Label string

	// Detail is what the last check actually observed.
	//
	// The reason this type exists rather than a status word. "Points elsewhere"
	// is not actionable; "points at ghs.googlehosted.com" is, and the difference
	// is the whole of the user's complaint about waiting without feedback.
	Detail string

	// At is when this stage last changed. Zero renders nothing rather than the
	// epoch.
	At time.Time

	State StepState
}

// DNSRecord is a record somebody has to create at their provider.
//
// Three fields because that is what every provider's form asks for. It was two
// paragraphs of prose before, which is the wrong shape for a value that gets
// retyped into another system.
type DNSRecord struct {
	Type  string
	Name  string
	Value string
}
