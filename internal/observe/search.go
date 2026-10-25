package observe

import "strings"

// Search is what a person typed into the log explorer's filter box, read.
//
// Railway's syntax, as far as it goes here: @service:name narrows to one of
// the project's services, @level:error to the lines that report an error, and
// everything else is text to find in the message. A filter is read into
// fields rather than passed through, so that it can only ever pick among the
// services the engine already put in the scope — @service:other-teams-app
// names nothing in the scope and narrows to nothing.
type Search struct {
	Text       string
	Services   []string
	ErrorsOnly bool
}

// ParseSearch reads a filter box's contents.
func ParseSearch(raw string) Search {
	var s Search
	var words []string
	for _, w := range strings.Fields(raw) {
		key, value, ok := strings.Cut(w, ":")
		if ok && strings.HasPrefix(key, "@") && value != "" {
			switch strings.ToLower(key) {
			case "@service":
				s.Services = append(s.Services, strings.ToLower(value))
				continue
			case "@level":
				if v := strings.ToLower(value); v == "error" || v == "err" {
					s.ErrorsOnly = true
					continue
				}
			}
		}
		words = append(words, w)
	}
	s.Text = strings.Join(words, " ")
	return s
}
