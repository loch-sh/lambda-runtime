// Test function for provided.al2023: a Go bootstrap built with
// aws-lambda-go, the way Go functions are deployed on Lambda.
package main

import (
	"context"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-lambda-go/lambdacontext"
)

type event struct {
	Name string `json:"name"`
}

func handle(ctx context.Context, e event) (map[string]string, error) {
	lc, _ := lambdacontext.FromContext(ctx)
	return map[string]string{"hello": e.Name, "requestId": lc.AwsRequestID, "runtime": "go"}, nil
}

func main() { lambda.Start(handle) }
