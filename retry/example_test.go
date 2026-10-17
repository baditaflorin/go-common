package retry_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/baditaflorin/go-common/retry"
)

// Only errors marked Transient are retried when IsTransient is the policy; any
// other error stops the loop at once.
func ExampleDo() {
	attempts := 0
	err := retry.Do(context.Background(), func(ctx context.Context) error {
		attempts++
		if attempts < 3 {
			return retry.Transient(errors.New("temporarily unavailable"))
		}
		return nil
	},
		retry.WithMaxAttempts(5),
		retry.WithBackoff(retry.NoBackoff()),
		retry.WithRetryIf(retry.IsTransient),
	)
	fmt.Println(attempts, err)
	// Output: 3 <nil>
}
