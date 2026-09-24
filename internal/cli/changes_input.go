package cli

import (
	"context"
	"os"

	"github.com/SamuelSupe/git-rg/internal/proposal"
)

func readChangePlan(ctx context.Context, filename string) (proposal.Plan, error) {
	type decoded struct {
		plan proposal.Plan
		err  error
	}
	result := make(chan decoded, 1)
	stdin := os.Stdin
	go func() {
		input := stdin
		if filename != "-" {
			var err error
			input, err = os.Open(filename)
			if err != nil {
				result <- decoded{err: err}
				return
			}
			defer input.Close()
		}
		stopClose := context.AfterFunc(ctx, func() { _ = input.Close() })
		defer stopClose()
		plan, err := proposal.Decode(input)
		result <- decoded{plan, err}
	}()
	// Some OS file opens/reads cannot be interrupted. The CLI must still exit on
	// cancellation; a buffered result also lets a late read finish without waiting.
	select {
	case <-ctx.Done():
		return proposal.Plan{}, ctx.Err()
	case value := <-result:
		if err := ctx.Err(); err != nil {
			return proposal.Plan{}, err
		}
		return value.plan, value.err
	}
}
