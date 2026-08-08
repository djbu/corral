package supervisor

import "testing"

// TestInstallAttachmentTakeOverMatrix is design doc §5.5's take-over
// decision table as a pure unit test against installAttachment itself —
// no PTY, no goroutines, no store — unlike TestTakeOver in
// attach_test.go, which exercises the same rule end-to-end through a
// real Registry.Attach call over net.Pipe. Both are worth having: this
// one pins down the exact four cells of the matrix without depending on
// fakeclaude timing; attach_test.go's version proves the wire-level
// consequences (a Detached frame, an Attach() return) actually happen.
func TestInstallAttachmentTakeOverMatrix(t *testing.T) {
	cases := []struct {
		name          string
		hasIncumbent  bool
		takeOver      bool
		wantErr       bool
		wantIncumbent bool
	}{
		{name: "no incumbent, take_over=false", hasIncumbent: false, takeOver: false, wantErr: false, wantIncumbent: false},
		{name: "no incumbent, take_over=true", hasIncumbent: false, takeOver: true, wantErr: false, wantIncumbent: false},
		{name: "incumbent, take_over=false", hasIncumbent: true, takeOver: false, wantErr: true, wantIncumbent: false},
		{name: "incumbent, take_over=true", hasIncumbent: true, takeOver: true, wantErr: false, wantIncumbent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Registry{}
			ls := &LiveSession{}
			var incumbent *Attachment
			if tc.hasIncumbent {
				incumbent = &Attachment{}
				ls.Attachment = incumbent
			}
			newAtt := &Attachment{}

			got, err := r.installAttachment(ls, newAtt, tc.takeOver)

			if tc.wantErr {
				if err != ErrAlreadyAttached {
					t.Fatalf("err = %v, want ErrAlreadyAttached", err)
				}
				// A rejected install must leave the incumbent installed —
				// the whole point of §5.5's polite-attacher rejection.
				if ls.Attachment != incumbent {
					t.Fatalf("ls.Attachment = %p, want unchanged incumbent %p", ls.Attachment, incumbent)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if ls.Attachment != newAtt {
				t.Fatalf("ls.Attachment = %p, want newly installed %p", ls.Attachment, newAtt)
			}
			if tc.wantIncumbent {
				if got != incumbent {
					t.Fatalf("returned incumbent = %p, want %p", got, incumbent)
				}
			} else if got != nil {
				t.Fatalf("returned incumbent = %p, want nil", got)
			}
		})
	}
}

// TestClearAttachmentIfCurrent verifies the guard that stops a stale
// eviction from clobbering a newer take-over (attach.go's doc comment on
// clearAttachmentIfCurrent): only the attachment that is still current
// gets cleared.
func TestClearAttachmentIfCurrent(t *testing.T) {
	r := &Registry{}
	ls := &LiveSession{}
	first := &Attachment{}
	second := &Attachment{}

	ls.Attachment = first
	r.clearAttachmentIfCurrent(ls, second) // stale: second was never installed.
	if ls.Attachment != first {
		t.Fatalf("clearAttachmentIfCurrent(stale) cleared ls.Attachment, want it left as %p", first)
	}

	r.clearAttachmentIfCurrent(ls, first) // current: must clear.
	if ls.Attachment != nil {
		t.Fatalf("ls.Attachment = %p after clearing current, want nil", ls.Attachment)
	}
}
