// Test function for provided.al2023: a Go bootstrap built with
// aws-lambda-go, the way Go functions are deployed on Lambda. It is also
// the Go example of the loch CLI tests (see README.md).
package main

import (
	"context"
	"errors"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
)

type event struct {
	Name string `json:"name"`
	// Op "throw" returns an error: a function error ("Unhandled").
	Op string `json:"op"`
}

func handle(ctx context.Context, e event) (map[string]string, error) {
	if e.Op == "throw" {
		return nil, errors.New("thrown on purpose")
	}
	lc, _ := lambdacontext.FromContext(ctx)
	return map[string]string{"hello": e.Name, "requestId": lc.AwsRequestID, "runtime": "go"}, nil
}

func main() { lambda.Start(handle) }
