// Package kind defines tether's message kinds: the shared vocabulary the
// store and the CLI both validate against, without either depending on the
// other's package -- the store needs no wire-protocol knowledge, and the
// CLI needs no SQLite dependency just to know the four kind names.
package kind

// The four message kinds.
const (
	Note     = "note"
	Handoff  = "handoff"
	Question = "question"
	Answer   = "answer"
)

// All lists every kind, in the order shown in help text and errors.
var All = []string{Note, Handoff, Question, Answer}

// Valid reports whether k is a known message kind.
func Valid(k string) bool {
	switch k {
	case Note, Handoff, Question, Answer:
		return true
	}
	return false
}
