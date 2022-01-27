//go:build integration

package subscriber_integration_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"
	"github.com/joho/godotenv"
	"github.com/mosuka/dynamodbstreams-subscriber/subscriber"
)

const (
	partitionKeyName = "pk"
	sortKeyName      = "sk"
	partitionValue   = "partition"
)

type kv struct {
	Partition string `dynamodbav:"pk"`
	Path      string `dynamodbav:"sk"`
	Version   string `dynamodbav:"version"`
	Value     string `dynamodbav:"value"`
}

func newAwsConfig(ctx context.Context) (aws.Config, error) {
	cfg := aws.Config{}

	profile := os.Getenv("AWS_PROFILE")
	if profile != "" {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
		if err != nil {
			return cfg, err
		}
	} else {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return cfg, err
		}
	}

	useCredentials := false
	accessKeyId := os.Getenv("AWS_ACCESS_KEY_ID")
	if accessKeyId != "" {
		useCredentials = true
	}

	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if secretAccessKey != "" {
		useCredentials = true
	}

	sessionToken := os.Getenv("AWS_SESSION_TOKEN")
	if sessionToken != "" {
		useCredentials = true
	}

	if useCredentials {
		cfg.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKeyId, secretAccessKey, sessionToken))
	}

	region := os.Getenv("AWS_DEFAULT_REGION")
	if region != "" {
		cfg.Region = region
	}

	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	if endpoint != "" {
		customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				PartitionID:   "aws",
				URL:           endpoint,
				SigningRegion: cfg.Region,
			}, nil
		})
		cfg.EndpointResolverWithOptions = customResolver
	}

	return cfg, nil
}

func createTable(ctx context.Context, dynamoSvc *dynamodb.Client, table string) error {
	input := &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
			{
				AttributeName: aws.String(partitionKeyName),
				AttributeType: dynamodbtypes.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String(sortKeyName),
				AttributeType: dynamodbtypes.ScalarAttributeTypeS,
			},
		},
		KeySchema: []dynamodbtypes.KeySchemaElement{
			{
				AttributeName: aws.String(partitionKeyName),
				KeyType:       dynamodbtypes.KeyTypeHash,
			},
			{
				AttributeName: aws.String(sortKeyName),
				KeyType:       dynamodbtypes.KeyTypeRange,
			},
		},
		BillingMode: dynamodbtypes.BillingModePayPerRequest,
		StreamSpecification: &dynamodbtypes.StreamSpecification{
			StreamEnabled:  aws.Bool(true),
			StreamViewType: dynamodbtypes.StreamViewTypeNewAndOldImages,
		},
	}

	if _, err := dynamoSvc.CreateTable(ctx, input); err != nil {
		var riue *dynamodbtypes.ResourceInUseException
		if errors.As(err, &riue) {
			return nil
		}

		return err
	}

	return nil
}

func put(ctx context.Context, dynamoSvc *dynamodb.Client, table string, key string, value string) error {
	rec := &kv{
		Partition: partitionValue,
		Path:      key,
		Value:     base64.RawURLEncoding.EncodeToString([]byte(value)),
	}

	attr, err := attributevalue.MarshalMap(rec)
	if err != nil {
		return err
	}

	_, err = dynamoSvc.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(table),
		Item:      attr,
	})
	if err != nil {
		return err
	}

	return nil
}

func TestNewStreamSubscriber(t *testing.T) {
	if err := godotenv.Load(filepath.FromSlash("../.env")); err != nil {
		t.Errorf("Failed to load .env file")
	}

	ctx := context.Background()

	cfg, err := newAwsConfig(ctx)
	if err != nil {
		t.Fatalf("error %v\n", err)
	}

	dynamoSvc := dynamodb.NewFromConfig(cfg)
	streamSvc := dynamodbstreams.NewFromConfig(cfg)

	table := "test"

	if err := createTable(ctx, dynamoSvc, table); err != nil {
		t.Fatalf("error %v\n", err)
	}

	done := make(chan bool)

	streamSubscriber := subscriber.NewStreamSubscriber(dynamoSvc, streamSvc, table)
	eventCh, errCh := streamSubscriber.GetStreamDataAsync()
	// eventCh, errCh := streamSubscriber.GetStreamData()
	eventList := make([]*types.Record, 0)

	go func() {
		for {
			select {
			case cancel := <-done:
				// check
				if cancel {
					fmt.Println("cancel")
					return
				}
			case event := <-eventCh:
				fmt.Println("event: ", event)
				eventList = append(eventList, event)
			case err := <-errCh:
				fmt.Println("err:", err)
				// default:
				// 	fmt.Println("no data")
			}
		}
	}()

	put(ctx, dynamoSvc, table, "key1", "value1")
	put(ctx, dynamoSvc, table, "key2", "value2")

	// wait for events to be processed
	time.Sleep(3 * time.Second)

	done <- true

	actual := len(eventList)
	expected := 2
	if actual != expected {
		t.Fatalf("expected %v, but %v\n", expected, actual)
	}
}
