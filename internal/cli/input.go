package cli

import (
	"context"
	"io"
	"os"

	"github.com/SamuelSupe/git-rg/internal/proposal"
)

func readChangePlan(ctx context.Context, filename string) (proposal.Plan, error) {
	return readCommandInput(ctx, filename, proposal.Decode)
}

func readCommandInput[T any](ctx context.Context, filename string, decode func(io.Reader) (T, error)) (T, error) {
	type decoded struct {
		value T
		err   error
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
		value, err := decode(input)
		result <- decoded{value, err}
	}()
	// Some OS file opens/reads cannot be interrupted. The CLI must still exit on
	// cancellation; a buffered result also lets a late read finish without waiting.
	select {
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	case value := <-result:
		if err := ctx.Err(); err != nil {
			var zero T
			return zero, err
		}
		return value.value, value.err
	}
}
