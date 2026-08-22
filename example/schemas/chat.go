package schemas

import (
	"badele"
)

// ChatIn is the prompt a chat stream answers.
//
// It is an ordinary JSON body: an event stream reached by POST binds and
// validates one exactly as any other route does, which is what a protocol that
// streams its answer to a posted document needs.
type ChatIn struct {
	// Text is what to answer.
	Text string `json:"text" doc:"The prompt to answer, one token at a time"`
}

// Validate bounds the prompt, and is applied before a byte of the stream is
// written.
func (in *ChatIn) Validate(v *badele.Validation) {
	v.String(&in.Text).Trim().Required().MinLen(1).MaxLen(280)
}
