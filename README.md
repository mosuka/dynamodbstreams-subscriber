# dynamodbstreams-subscriber
Go channel for streaming DynamoDB Updates

Forked from urakozz/go-dynamodb-stream-subscriber.

```go
package main

import (
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	"github.com/mosuka/dynamodbstreams-subscriber/subscriber"
)

func main() {
	ctx := context.Background()
	cfg, _ = config.LoadDefaultConfig(ctx)
	streamSvc := dynamodb.NewFromConfig(cfg)
	dynamoSvc := dynamodbstreams.NewFromConfig(cfg)
	table := "tableName"

	streamSubscriber := subscriber.NewStreamSubscriber(dynamoSvc, streamSvc, table)
	ch, errCh := streamSubscriber.GetStreamDataAsync()

	go func(errCh <-chan error) {
		for err := range errCh {
			fmt.Println("Stream Subscriber error: ", err)
		}
	}(errCh)

	for record := range ch {
		fmt.Println("from channel:", record)
	}
}
```
